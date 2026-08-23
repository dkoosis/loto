package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/exec"
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

A listed file can reference a symbol that lives only in an unlisted, uncommitted
file — the tree you verified locally is not the tree that lands (loto-5aug).
Every lane warns when an untracked file shares a directory with a listed file;
pass --build to also build the committed ref in a throwaway worktree before
returning (slower, off by default).

examples:
  loto lane internal/store/store.go --ref impl-1 --base main -m "store: fix X" --closes loto-abc
  loto lane a.go b.go --ref impl-1 --base main -m "two files" --closes "loto-abc, loto-def"
  loto lane a.go --ref impl-1 --base main -m "fix" --closes loto-abc --build
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
	build := fs.Bool("build", false, "build the committed ref (go build ./...) in a throwaway worktree before returning; slower, catches what the default untracked-sibling warning misses")
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

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	return runLaneCommit(rt, repoTop, *ref, *base, *msg, *closes, targets, *build, stdout, stderr)
}

// runLaneCommit brackets lane.Commit with a lock assertion on both sides so the
// caller cannot stage a path it does not exclusively hold, and so a lock lost
// across the stage (a peer reclaim) is caught. The lane.go doc states the engine
// cannot close that TOCTOU window alone; this is the CLI half that closes it.
func runLaneCommit(rt *runtime, repoTop, ref, base, msg, closes string, targets []domain.Target, build bool, stdout, stderr io.Writer) int {
	owner := domain.AgentUUID(rt.Agent.UUID)
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}

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
	//
	// This runs BEFORE the sibling scan below, not after (codex #286 finding 3):
	// the scan shells out to `git status`, which can burn up to gitTimeout (30s)
	// on a wedged repo. Sampling lock state only after that window would let a
	// lease that expired DURING the scan — after staging genuinely finished —
	// report lane-tainted for a transition that provably could not have touched
	// the recorded tree.
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

	// loto-5aug: the write-set the caller listed and the working tree they
	// verified can legitimately differ — that's what a lane is for. Warn
	// (never block) when a file in a listed file's directory is absent from
	// the commit's own tree: the commit may reference a symbol that exists
	// only there. The scan is asked against commit^ — the ACTUAL parent
	// buildLaneTree seeded from (Base, or an earlier wave's tip; commit^ is
	// right either way, no first-wave/later-wave branch needed here) — not
	// against Base directly, since dk's #286 review found `git status` alone
	// missed a sibling committed at HEAD after Base: `--base main` from a
	// checkout ahead of main is ordinary usage, and status reports relative
	// to HEAD, not Base. A scan failure is non-fatal — the commit already
	// landed on the lane ref, and turning an advisory into a way to fail
	// every lane would cost more than it warns about.
	if parent, perr := gitResolveCommit(rt.Ctx, repoTop, commit+"^"); perr != nil {
		fmt.Fprintf(stdout, "⚠ lane sibling-scan failed ref=loto/%s commit=%s: resolve parent: %v\n", ref, commit, perr)
	} else if siblings, serr := lane.SiblingUntracked(rt.Ctx, repoTop, parent, writeSet); serr != nil {
		fmt.Fprintf(stdout, "⚠ lane sibling-scan failed ref=loto/%s commit=%s: %v\n", ref, commit, serr)
	} else if len(siblings) > 0 {
		emitLaneUnlistedSiblings(stdout, ref, commit, siblings)
	}

	if build {
		res, verr := lane.Verify(rt.Ctx, repoTop, commit, []string{"go", "build", "./..."})
		if verr != nil {
			fmt.Fprintf(stderr, "✗ lane build-check aborted commit=%s: %v\n", commit, verr)
			return 3
		}
		if !res.Passed {
			emitLaneBuildFailed(rt.Ctx, stdout, stderr, repoTop, ref, base, commit, res.Output)
			return 1
		}
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
	locks, err := rt.Store.LocksForOwnerAt(rt.Ctx, targets, owner)
	if err != nil {
		return nil, nil, fmt.Errorf("lock query: %w", err)
	}
	held := make(map[string]time.Time, len(targets))
	var blocked []laneBlock
	for i := range targets {
		t := targets[i]
		l, ok := locks[t.Canonical]
		switch {
		case !ok:
			blocked = append(blocked, laneBlock{t.Canonical, "no-lock-held"})
		case ec.IsStale(l):
			blocked = append(blocked, laneBlock{t.Canonical, lockStaleReason})
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
	locks, err := rt.Store.LocksForOwnerAt(rt.Ctx, targets, owner)
	if err != nil {
		return nil, fmt.Errorf("lock recheck: %w", err)
	}
	var tainted []laneBlock
	for i := range targets {
		t := targets[i]
		l, ok := locks[t.Canonical]
		switch {
		case !ok:
			tainted = append(tainted, laneBlock{t.Canonical, "lock-lost"})
		case ec.IsStale(l):
			tainted = append(tainted, laneBlock{t.Canonical, lockStaleReason})
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

// emitLaneUnlistedSiblings renders SiblingUntracked's findings (loto-5aug): a
// non-fatal advisory, never a refusal — the ref is already committed and the
// finding may be nothing (an unrelated new scratch file in the same
// directory). ✓ glyph is ⚠, not ✗, matching design.md's non-fatal-advisory row.
//
// The reason token names staged vs untracked explicitly (dk review on #286):
// `git status` on a staged sibling shows "A  path", and a row that still says
// "untracked" reads as the tool looking at something else — a reader who
// checks git status against a wrong reason wastes a whole investigation on
// nothing. The distinction is also actionable, not just cosmetic: untracked
// needs staging before it can be listed; staged needs only listing.
func emitLaneUnlistedSiblings(w io.Writer, ref, commit string, siblings []lane.UnlistedSibling) {
	sort.Slice(siblings, func(i, j int) bool { return siblings[i].Path < siblings[j].Path })
	fmt.Fprintf(w, "⚠ lane-unlisted-new count=%d ref=loto/%s commit=%s\n", len(siblings), ref, commit)
	for _, s := range siblings {
		fmt.Fprintf(w, "⚠ target=%s dir=%s reason=%s\n", s.Path, s.Dir, siblingReason(s.Origin))
	}
	fmt.Fprintf(w, "ℹ a file this commit will not carry (untracked, staged, or committed after --base) shares a directory with a listed file and may hold a symbol commit %s references but never carries; list it (git add first if untracked), delete it, or confirm it's unrelated\n", commit)
	fmt.Fprintln(w, "ℹ for a stronger check: loto lane ... --build")
}

// siblingReason maps a SiblingOrigin to its row's reason token — an explicit
// case per value (exhaustive-checked) so a new SiblingOrigin can't silently
// fall through to the wrong wording.
func siblingReason(o lane.SiblingOrigin) string {
	switch o {
	case lane.OriginStaged:
		return "staged-new-sibling-not-in-write-set"
	case lane.OriginCommittedAfterParent:
		return "committed-after-base-sibling-not-in-write-set"
	case lane.OriginUntracked:
		return "untracked-sibling-not-in-write-set"
	}
	return "untracked-sibling-not-in-write-set"
}

// emitLaneBuildFailed renders a --build failure (loto-5aug thorough path): the
// commit already landed on the lane ref but `go build ./...` failed against it
// in a throwaway worktree, so the author learns before push rather than from CI.
// emitLaneBuildFailed renders a --build failure (loto-5aug thorough path): the
// commit already landed on the lane ref but `go build ./...` failed against it
// in a throwaway worktree, so the author learns before push rather than from CI.
//
// The fix block is a compare-and-swap against commit (the ref's known-bad
// current value), never a bare `-d` delete: resolveParent makes a lane's Nth
// wave a child of its (N-1)th, so refs/heads/loto/<ref> can carry earlier
// SUCCESSFUL waves this failed commit is stacked on top of. An unconditional
// delete would erase those too (codex #286 finding 2) — a fix block that
// loses work is worse than none, and an agent under time pressure will paste
// it without checking. laneBuildFailRemediation resolves the right shape; if
// it can't (git failure resolving parent/base), this prints an explicit "no
// safe command" notice rather than ever guessing wrong.
func emitLaneBuildFailed(ctx context.Context, w, errw io.Writer, repoTop, ref, base, commit, output string) {
	fmt.Fprintf(w, "✗ lane-build-failed ref=loto/%s commit=%s\n", ref, commit)
	if body := strings.TrimRight(output, "\n"); body != "" {
		fmt.Fprintln(w, body)
	}
	fixCmd, err := laneBuildFailRemediation(ctx, repoTop, ref, base, commit)
	if err != nil {
		fmt.Fprintf(errw, "⚠ lane build-fail remediation: %v\n", err)
		fmt.Fprintf(w, "ℹ could not compute a safe fix command; inspect refs/heads/loto/%s by hand before deleting it — it may carry earlier successful waves\n", ref)
		return
	}
	fmt.Fprintf(w, "```bash\n%s\n```\n", fixCmd)
}

// laneBuildFailRemediation returns the compare-and-swap `git update-ref`
// command that safely undoes a failed --build commit: reset to the commit's
// own parent when the ref carried a prior wave (preserving it), or an
// outright — still CAS-guarded — delete only when this was genuinely the
// first wave (parent equals the lane's own Base, so nothing preceded it on
// the ref). Returns an error, never a guess, if parent or base cannot be
// resolved — a wrong guess here is a silent data-loss command.
func laneBuildFailRemediation(ctx context.Context, repoTop, ref, base, commit string) (string, error) {
	parent, err := gitResolveCommit(ctx, repoTop, commit+"^")
	if err != nil {
		return "", fmt.Errorf("resolve %s^: %w", commit, err)
	}
	baseSHA, err := gitResolveCommit(ctx, repoTop, base+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("resolve base %q: %w", base, err)
	}
	refName := "refs/heads/loto/" + ref
	if parent == baseSHA {
		// First wave: nothing preceded this commit on the ref. Deleting
		// restores the pre-commit state (the ref did not exist), not merely
		// "some earlier tip" — still CAS-guarded against commit.
		return fmt.Sprintf("git update-ref -d %s %s", refName, commit), nil
	}
	// A prior successful wave sits at parent; reset to it instead of deleting
	// so that wave survives. CAS-guarded: a no-op if something else already
	// moved the ref off commit.
	return fmt.Sprintf("git update-ref %s %s %s", refName, parent, commit), nil
}

// gitResolveCommit resolves rev to a commit SHA in repoTop, or an error if it
// does not resolve. gitTimeout (runtime.go) bounds it against a wedged repo.
func gitResolveCommit(ctx context.Context, repoTop, rev string) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	c := exec.CommandContext(cctx, "git", "rev-parse", "--verify", "--quiet", rev)
	c.Dir = repoTop
	out, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s: %w", rev, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveLaneWriteSet canonicalizes each arg to a repo-relative target and
// rejects duplicates, mirroring lock's pre-store validation. Unlike lock it does
// not Lstat — a removed file is a legitimate deletion in a lane write-set —
// leaving on-disk shape checks to lane.validateWriteSet. Output is sorted by
// canonical path for deterministic staging and reporting.
func resolveLaneWriteSet(args []string, repoTop string) ([]domain.Target, []render.InvalidTarget) {
	// The canonicalize+dedupe loop intentionally mirrors validateLockTargets;
	// the divergence (no Lstat, sort after) is the point.
	/* jscpd:ignore-start */
	targets := make([]domain.Target, 0, len(args))
	seen := make(map[string]bool, len(args))
	var invalid []render.InvalidTarget
	base := callerBase()
	for _, raw := range args {
		t, err := resolveCLITarget(base, repoTop, raw)
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
	/* jscpd:ignore-end */
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
