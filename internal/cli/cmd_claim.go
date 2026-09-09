package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("claim", cmdClaim) } //nolint:gochecknoinits // command registry pattern

// claimUsageHead is the point-of-use teaching surface for claim (mirrors
// lockUsageHead). The flag list is appended by PrintDefaults.
const claimUsageHead = `usage: loto claim <path-prefix> -t "why" [--ttl 2h]

Atomically reserve a repo-relative directory prefix as session territory:
"this package is mine this session" — coarser than a per-file lock. Refused
while another agent's live claim overlaps the prefix (equal or ancestor/
descendant by path segment). Re-claiming your own prefix refreshes the TTL.
Advisory between claimants only: a claim does not block loto lock/check under
the prefix — still lock files before editing.

"." is the repo root, the widest prefix: it overlaps every other claim, so it
blocks every peer claimant until it expires or you unclaim it. Reach for it
deliberately — a takeover of the whole checkout — not as a shorthand for "the
directory I am standing in": a prefix is always repo-root-relative, never
cwd-relative. loto lock still refuses ".", because a lock names a write-set.

examples:
  loto claim internal/store -t "store refactor"
  loto claim pkg/newthing -t "scaffolding a new package" --ttl 2h
  loto claim . -t "takeover: rebasing every lane" --ttl 30m
`

func cmdClaim(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("claim", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Usage = func() {
		fmt.Fprint(stderr, claimUsageHead)
		flags.PrintDefaults()
	}
	ttl := flags.Duration("ttl", 2*time.Hour, "claim TTL (session-scale lease)")
	intent := flags.String("t", "", "intent (required)")
	flags.StringVar(intent, "intent", "", "intent (required)")
	if err := flags.Parse(permuteWith(flags, args)); err != nil {
		return 2
	}
	if *intent == "" {
		fmt.Fprintln(stderr, "✗ -t required: loto claim <path-prefix> -t \"why\"")
		return 2
	}
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be positive, got %s\n", *ttl)
		return 2
	}
	prefix, repoTop, code := claimVerbPrefix(ctx, flags, "usage: loto claim <path-prefix> -t \"why\" [--ttl 2h]", stderr)
	if code != 0 {
		return code
	}
	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	defer rt.DeferredTagFooter(stdout)

	now := time.Now()
	rec := domain.ClaimRecord{
		PathPrefix:  prefix.Canonical,
		OwnerUUID:   domain.AgentUUID(rt.Agent.UUID),
		SessionUUID: rt.SessionUUID,
		Intent:      *intent,
		CreatedAt:   now,
		ExpiresAt:   now.Add(*ttl),
		Host:        rt.Host,
	}
	// memoLiveProbe: the partition evaluates the predicate once per overlapping
	// row, and several rows commonly share one dead owner (Codex #246).
	if err := rt.Store.ClaimPrefix(rt.Ctx, rec, memoLiveProbe(rt.liveProbe())); err != nil {
		var cce *store.ClaimConflictError
		if errors.As(err, &cce) {
			render.EmitClaimConflict(stdout, cce)
			return 1
		}
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	render.EmitClaimSuccess(stdout, rec)
	emitNotOnDiskAdvisory(stdout, repoTop, prefix.Canonical)
	recordRefRefusalOverride(rt, prefix.Canonical, now, stderr)
	return 0
}

// recordRefRefusalOverride writes the ref_refused_overridden counter when this
// claim is the operator overriding I1 — a checkout-wide claim taken by the
// same owner within refOverrideWindow of a ref_refused (§10b row 1).
//
// The counter lives here rather than in the hook because the OVERRIDE is the
// claim, not the refusal: the hook exits before the operator has decided
// anything. Reading refusals / overrides per week is what decides whether the
// three-shape denylist is refusing the right things or whether `allow` itself
// is wrong.
//
// Three things must line up before a claim counts as an override, and each
// one was a way to inflate the ratio:
//
//   - the ACTOR matches — a peer's refusal says nothing about why this owner
//     is claiming the checkout;
//   - the CHECKOUT matches — loto's store is per project, so two worktrees of
//     one repo share it and a refusal in the other one is not this claim's;
//   - the refusal is UNCONSUMED — an override already written for it means a
//     second `loto claim .` inside the same ten minutes would otherwise count
//     the one refusal twice, which is exactly the direction that makes
//     overrides look like refusals and condemns `allow` on a miscount.
//
// "Unconsumed" needs no new column: an override event newer than the refusal
// IS the consumed marker, and both are already in the events table.
//
// Best-effort throughout: a claim is a coordination write, and losing a
// telemetry row must never fail it.
func recordRefRefusalOverride(rt *runtime, prefix string, now time.Time, stderr io.Writer) {
	if prefix != refCheckoutWidePrefix {
		return
	}
	since := now.Add(-refOverrideWindow)
	refusal, ok, err := rt.Store.LatestEventByKindActor(
		rt.Ctx, store.EventRefRefused, rt.Agent.UUID, since)
	if err != nil || !ok {
		return
	}
	if !refRefusalInCheckout(refusal.Detail, rt.RepoTop) {
		return
	}
	prior, hadPrior, perr := rt.Store.LatestEventByKindActor(
		rt.Ctx, store.EventRefRefusedOverridden, rt.Agent.UUID, since)
	if perr != nil {
		return
	}
	if hadPrior && !prior.CreatedAt.Before(refusal.CreatedAt) {
		return // already consumed by an earlier claim
	}
	if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
		Kind:      store.EventRefRefusedOverridden,
		Target:    refusal.Target,
		ActorUUID: rt.Agent.UUID,
		Reason:    refusal.Reason,
		Detail:    refusal.Detail,
		CreatedAt: now,
	}); err != nil {
		fmt.Fprintf(stderr, "⚠ ref-override-counter=unrecorded err=%q\n", err)
	}
}

// refRefusalInCheckout reports whether a ref_refused event's payload names
// repoTop. A refusal written before the repo field existed carries none; it
// is NOT paired, because pairing it would attribute an override to a checkout
// nobody can show it happened in.
func refRefusalInCheckout(detail, repoTop string) bool {
	var d refRefusedDetail
	if json.Unmarshal([]byte(detail), &d) != nil {
		return false
	}
	return d.Repo != "" && d.Repo == repoTop
}

// resolveCLIPrefix normalizes a user-supplied prefix (absolute inside the
// repo, relative, trailing slash) into a canonical claim prefix — the prefix
// counterpart of resolveCLITarget, sharing normalizeRepoPath so one policy
// governs path translation.
//
// The case fold runs after CanonicalizePrefix trims the trailing slash, so the
// key never carries an empty last segment. Claims fold for the same reason
// locks do (loto-f8m8): PrefixOverlaps is a byte comparison, so a claim on
// internal/Store did not cover a lock on internal/store/x.go.
func resolveCLIPrefix(cc *caseCache, repoTop, raw string) (domain.Target, error) {
	t, err := domain.CanonicalizePrefix(normalizeRepoPath(raw, repoTop))
	if err != nil {
		return domain.Target{}, err
	}
	t.Canonical = foldTargetKey(cc, repoTop, t.Canonical)
	return t, nil
}

// claimVerbPrefix is the shared arg preamble of the claim/unclaim verb pair:
// exactly one <path-prefix> positional, canonicalized via resolveCLIPrefix.
// Returns code 2 on usage/invalid-prefix failure; repoTop rides along so
// claim's not-on-disk advisory doesn't re-derive it.
func claimVerbPrefix(ctx context.Context, flags *flag.FlagSet, usage string, stderr io.Writer) (domain.Target, string, int) {
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, usage)
		return domain.Target{}, "", 2
	}
	repoTop, _ := repoTopForCwd(ctx)
	prefix, err := resolveCLIPrefix(nil, repoTop, flags.Arg(0))
	if err != nil {
		render.EmitInvalid(stderr, []render.InvalidTarget{{Path: flags.Arg(0), Reason: classifyCanonicalizeErr(err)}})
		return domain.Target{}, "", 2
	}
	return prefix, repoTop, 0
}

// emitNotOnDiskAdvisory prints the ⚠ row when the claimed prefix does not
// exist on disk. Advisory only — claim-before-scaffold (reserving a package
// about to be created) is a legitimate flow, so the claim landed regardless.
// The stat lives here in the cli layer: store never stats, domain stays
// disk-free (loto-claim plan, arch-fit).
func emitNotOnDiskAdvisory(w io.Writer, repoTop, canonical string) {
	p := canonical
	if repoTop != "" {
		p = filepath.Join(repoTop, canonical)
	}
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(w, "⚠ prefix=%s not-on-disk\n", canonical)
	}
}
