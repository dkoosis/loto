package cli

import (
	"context"
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

examples:
  loto claim internal/store -t "store refactor"
  loto claim pkg/newthing -t "scaffolding a new package" --ttl 2h
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
	if err := rt.Store.ClaimPrefix(rt.Ctx, rec); err != nil {
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
	return 0
}

// resolveCLIPrefix normalizes a user-supplied prefix (absolute inside the
// repo, relative, trailing slash) into a canonical claim prefix — the prefix
// counterpart of resolveCLITarget, sharing normalizeRepoPath so one policy
// governs path translation.
func resolveCLIPrefix(repoTop, raw string) (domain.Target, error) {
	return domain.CanonicalizePrefix(normalizeRepoPath(raw, repoTop))
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
	prefix, err := resolveCLIPrefix(repoTop, flags.Arg(0))
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
