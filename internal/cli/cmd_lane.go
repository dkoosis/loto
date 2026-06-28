package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/lane"
	"loto/internal/render"
)

func init() { register("lane", cmdLane) } //nolint:gochecknoinits // command registry pattern

const laneUsageHead = `usage: loto lane <file> [<file>...] --ref <name> --base <commit-ish> -m "<msg>" --closes "<ids>"

Commit an exact write-set to refs/heads/loto/<ref> by git plumbing — no checkout,
no HEAD move. Refuses unless THIS identity holds an exclusive loto lock on every
listed file, held across staging. The commit message gains a Closes: trailer.

examples:
  loto lane internal/store/store.go --ref impl-1 --base main -m "store: fix X" --closes loto-abc
  loto lane a.go b.go --ref impl-1 --base main -m "two files" --closes "loto-abc, loto-def"
`

// laneAfterPreAssert is a test seam. When non-nil it runs after the write-set
// lock pre-assertion passes and before lane.Commit stages, receiving the live
// runtime so a test can mutate lock state inside the TOCTOU window and so
// exercise the post-commit re-assertion deterministically. Nil in production
// (mirrors the package-level hook pattern in internal/identity).
var laneAfterPreAssert func(*runtime) //nolint:gochecknoglobals // test seam, production-nil

// laneBlock is one write-set path that failed a lock assertion, with the reason
// token the Claude-optimized report prints.
type laneBlock struct {
	Path   string
	Reason string
}

func cmdLane(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lane", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, laneUsageHead)
		fs.PrintDefaults()
	}
	ref := fs.String("ref", "", "lane name → refs/heads/loto/<ref> (required)")
	base := fs.String("base", "", "base commit-ish the lane forks from (required)")
	msg := fs.String("m", "", "commit message (required)")
	fs.StringVar(msg, "message", "", "commit message (required)")
	closes := fs.String("closes", "", `Closes: trailer ids, e.g. "loto-abc, loto-def" or "none" (required)`)
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if missing := laneMissingFlag(*ref, *base, *msg, *closes); missing != "" {
		fmt.Fprintf(stderr, "✗ %s required\n", missing)
		fmt.Fprint(stderr, laneUsageHead)
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(stderr, `✗ write-set required: loto lane <file> [<file>...] --ref <name> --base <commit-ish> -m "<msg>" --closes "<ids>"`)
		return 2
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	// Resolve the write-set to canonical repo-relative targets. No on-disk
	// regular-file check: a lane legitimately commits a deletion of a removed
	// file, and the lock query keys on the canonical path regardless of disk
	// state. lane.Commit re-validates the write-set shape (dir/glob/escape).
	targets, invalid := resolveLaneWriteSet(fs.Args(), repoTop)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	return runLaneCommit(rt, repoTop, *ref, *base, *msg, *closes, targets, stdout, stderr)
}

// runLaneCommit brackets lane.Commit with a lock assertion on both sides so the
// caller cannot stage a path it does not exclusively hold, and so a lock lost
// across the stage (a peer reclaim) is caught. The lane.go doc states the engine
// cannot close that TOCTOU window alone; this is the CLI half that closes it.
func runLaneCommit(rt *runtime, repoTop, ref, base, msg, closes string, targets []domain.Target, stdout, stderr io.Writer) int {
	owner := domain.AgentUUID(rt.Agent.UUID)
	ec := domain.EvalContext{Now: time.Now(), ThisHost: rt.Host, Live: rt.liveProbe()}

	// Pre-assert: every write-set path must be held by THIS identity under a
	// live exclusive lock. A single unheld path refuses the whole commit and
	// writes no lane ref. A store/ctx error is not evidence about lock ownership,
	// so it aborts as infra (exit 3) rather than masquerading as a blocked path.
	heldAt, blocked, err := assertLocksHeld(rt, ec, targets, owner)
	if err != nil {
		fmt.Fprintf(stderr, "✗ lane lock-check: %v\n", err)
		return 3
	}
	if len(blocked) > 0 {
		emitLaneBlocked(stdout, ref, blocked)
		return 1
	}

	if laneAfterPreAssert != nil {
		laneAfterPreAssert(rt)
	}

	writeSet := make([]string, len(targets))
	for i := range targets {
		writeSet[i] = targets[i].Canonical
	}
	id := laneIdentity(rt.Agent)
	commit, err := lane.Commit(rt.Ctx, lane.Opts{
		RepoTop:   repoTop,
		Base:      base,
		Ref:       ref,
		WriteSet:  writeSet,
		Message:   buildLaneMessage(msg, closes),
		Author:    id,
		Committer: id,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ lane commit: %v\n", err)
		return 3
	}

	// Post-assert: re-check the same locks are still held, live, exclusive, and
	// the SAME lock instance (acquire-time unchanged). A discrepancy means the
	// hold did not span staging — the commit may carry a peer's edit, so report
	// the lane tainted and point at the ref to discard.
	ec.Now = time.Now()
	tainted, err := reassertLocksHeld(rt, ec, targets, owner, heldAt)
	if err != nil {
		// Could not re-read lock state — infra, NOT taint. The commit ref exists
		// but its provenance is unconfirmed; advising the operator to delete a
		// possibly-valid commit on a transient store error would be wrong.
		fmt.Fprintf(stderr, "✗ lane verify-locks commit=%s: %v\n", commit, err)
		return 3
	}
	if len(tainted) > 0 {
		emitLaneTainted(stdout, ref, commit, tainted)
		return 1
	}

	fmt.Fprintf(stdout, "✓ lane committed ref=loto/%s commit=%s files=%d\n", ref, commit, len(writeSet))
	return 0
}

// assertLocksHeld verifies owner holds a live exclusive loto lock on every
// target. It returns each held lock's acquire-time keyed by canonical path (the
// stability snapshot the post-commit re-check compares against) and the paths
// that fail the precondition. A store/ctx read error is returned as a third
// value — it is not evidence about lock ownership, so the caller treats it as
// infra (exit 3) rather than a blocked path.
func assertLocksHeld(rt *runtime, ec domain.EvalContext, targets []domain.Target, owner domain.AgentUUID) (map[string]time.Time, []laneBlock, error) {
	held := make(map[string]time.Time, len(targets))
	var blocked []laneBlock
	for i := range targets {
		t := targets[i]
		l, err := rt.Store.LockForOwnerAt(rt.Ctx, t, owner)
		switch {
		case err != nil:
			return nil, nil, fmt.Errorf("lock query %s: %w", t.Canonical, err)
		case l == nil:
			blocked = append(blocked, laneBlock{t.Canonical, "no-lock-held"})
		case ec.IsStale(*l):
			blocked = append(blocked, laneBlock{t.Canonical, "lock-stale"})
		case l.EffectiveMode() != domain.ModeExclusive:
			blocked = append(blocked, laneBlock{t.Canonical, "lock-not-exclusive"})
		default:
			held[t.Canonical] = l.CreatedAt
		}
	}
	return held, blocked, nil
}

// reassertLocksHeld re-checks, after staging, that every lock the pre-assert
// accepted is still held by owner, still live, still exclusive, and is the same
// lock instance (CreatedAt unchanged). Any vanished/stale/downgraded/reacquired
// lock means a peer could have reclaimed the path and dirtied the working tree
// inside the stage window (loto-9sro TOCTOU). A store/ctx read error is returned
// as the second value: the re-check could not run, which is infra — distinct
// from a confirmed taint, so the caller must not advise discarding the commit.
func reassertLocksHeld(rt *runtime, ec domain.EvalContext, targets []domain.Target, owner domain.AgentUUID, before map[string]time.Time) ([]laneBlock, error) {
	var tainted []laneBlock
	for i := range targets {
		t := targets[i]
		l, err := rt.Store.LockForOwnerAt(rt.Ctx, t, owner)
		switch {
		case err != nil:
			return nil, fmt.Errorf("lock recheck %s: %w", t.Canonical, err)
		case l == nil:
			tainted = append(tainted, laneBlock{t.Canonical, "lock-lost"})
		case ec.IsStale(*l):
			tainted = append(tainted, laneBlock{t.Canonical, "lock-stale"})
		case l.EffectiveMode() != domain.ModeExclusive:
			tainted = append(tainted, laneBlock{t.Canonical, "lock-downgraded"})
		case !l.CreatedAt.Equal(before[t.Canonical]):
			tainted = append(tainted, laneBlock{t.Canonical, "lock-reacquired"})
		}
	}
	return tainted, nil
}

func emitLaneBlocked(w io.Writer, ref string, blocked []laneBlock) {
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].Path < blocked[j].Path })
	fmt.Fprintf(w, "✗ lane-blocked count=%d ref=loto/%s\n", len(blocked), ref)
	for _, b := range blocked {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", b.Path, b.Reason)
	}
	fmt.Fprintf(w, "ℹ lock the write-set first: loto lock <file>... -t \"why\"\n")
}

func emitLaneTainted(w io.Writer, ref, commit string, tainted []laneBlock) {
	sort.Slice(tainted, func(i, j int) bool { return tainted[i].Path < tainted[j].Path })
	fmt.Fprintf(w, "✗ lane-tainted count=%d ref=loto/%s commit=%s\n", len(tainted), ref, commit)
	for _, b := range tainted {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", b.Path, b.Reason)
	}
	fmt.Fprintf(w, "⚠ a lock did not hold across staging; commit %s may include edits made after it was lost\n", commit)
	fmt.Fprintf(w, "```bash\ngit update-ref -d refs/heads/loto/%s\n```\n", ref)
}

// resolveLaneWriteSet canonicalizes each arg to a repo-relative target and
// rejects duplicates, mirroring lock's pre-store validation. Unlike lock it does
// not Lstat — a removed file is a legitimate deletion in a lane write-set —
// leaving on-disk shape checks to lane.validateWriteSet. Output is sorted by
// canonical path for deterministic staging and reporting.
func resolveLaneWriteSet(args []string, repoTop string) ([]domain.Target, []render.InvalidTarget) {
	targets := make([]domain.Target, 0, len(args))
	seen := make(map[string]bool, len(args))
	var invalid []render.InvalidTarget
	for _, raw := range args {
		t, err := resolveCLITarget(repoTop, raw)
		if err != nil {
			invalid = append(invalid, render.InvalidTarget{Path: raw, Reason: classifyCanonicalizeErr(err)})
			continue
		}
		if seen[t.Canonical] {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "duplicate-target"})
			continue
		}
		seen[t.Canonical] = true
		targets = append(targets, t)
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].Canonical < targets[j].Canonical })
	return targets, invalid
}

// laneIdentity maps the loto agent to a git author/committer principal.
// commit-tree (used by lane.Commit) ignores git config, so both name and email
// must be explicit; the UUID-bearing email keeps the commit traceable to the
// exact agent record.
func laneIdentity(a *identity.Agent) lane.Identity {
	return lane.Identity{Name: a.Handle, Email: a.UUID + "@loto.local"}
}

// buildLaneMessage appends the repo's Closes: trailer to the body, separated by
// a blank line so git reads it as a trailer block.
func buildLaneMessage(msg, closes string) string {
	body := strings.TrimRight(msg, "\n")
	return body + "\n\nCloses: " + normalizeCloses(closes) + "\n"
}

// normalizeCloses splits comma/space-separated ids, trims, dedupes preserving
// order, and rejoins with ", ". An empty result renders "none" (the repo
// convention for a trailer with no bead).
func normalizeCloses(raw string) string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n'
	})
	seen := make(map[string]bool, len(fields))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		if seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

func laneMissingFlag(ref, base, msg, closes string) string {
	switch {
	case ref == "":
		return "--ref"
	case base == "":
		return "--base"
	case msg == "":
		return "-m/--message"
	case closes == "":
		return "--closes"
	}
	return ""
}
