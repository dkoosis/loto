package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

func init() { register("sync", cmdSync) } //nolint:gochecknoinits // command registry pattern

// syncUsageHead is the point-of-use teaching surface (loto-5rwc), φ submitUsageHead.
const syncUsageHead = `usage: loto sync

Fast-forward every unleased path that diverges from refs/loto/integration to
integration content, and report the conflicts it refused to touch.

Divergence comes from rejected/abandoned candidates leaving tree residue,
out-of-band writes, and interrupted sessions — never ordinary promotion
(promoted blobs already originate in the tree). A path under a live lease, a
live territory claim, or an unresolved candidate claim is never written; it
is reported as a conflict row instead.

loto sync takes no arguments; run it from anywhere inside the repo.
`

// syncState classifies one integration-tracked path's divergence shape.
type syncState int

const (
	syncModified syncState = iota
	syncMissing
	syncModeOnly
	syncUnsupported
)

// syncEntry is one row of refs/loto/integration's tree manifest (git ls-tree
// -r -z): the content integration says path should have.
type syncEntry struct {
	Path string
	Mode string
	OID  string
}

// syncDiff is a syncEntry the worktree disagrees with, plus how.
type syncDiff struct {
	syncEntry
	State syncState
}

// syncConflict is a divergent path syncDecide refused to write, and why.
// Holder is a uuid8 (leased/territory-claim) or a candidate id (candidate-claim).
type syncConflict struct {
	Path, Reason, Holder string
}

const (
	syncReasonLeased         = "leased"
	syncReasonTerritoryClaim = "territory-claim"
	syncReasonCandidateClaim = "candidate-claim"
)

// errHashObjectCountMismatch is batchHashObject's static sentinel — git
// hash-object returning a different oid count than the paths fed to it is
// infrastructure trouble (a stdin/stdout framing bug), not an expected verdict.
var errHashObjectCountMismatch = errors.New("git hash-object: oid count mismatch")

// errSyncIntegrationChanged refuses a stale repair plan when promotion moved
// refs/loto/integration after divergence was computed.
var errSyncIntegrationChanged = errors.New("sync: integration changed during repair")

// syncBeforeApplyFn pauses sync after its coordination-state read and decision.
// Nil in production; tests use it to make the lease-acquire TOCTOU deterministic.
var syncBeforeApplyFn func() //nolint:gochecknoglobals // production-nil concurrency test seam

// syncWriteFn indirects the temporary-file write so tests can force a short
// write before publication.
var syncWriteFn = func(f *os.File, data []byte) (int, error) { //nolint:gochecknoglobals // fault-injection seam
	return f.Write(data)
}

// syncParentDirFn is the post-rename durability seam. Tests force this final
// step to fail and assert the already-published path remains in the report.
var syncParentDirFn = syncParentDir //nolint:gochecknoglobals // fault-injection seam

func cmdSync(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, syncUsageHead)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, syncUsageHead)
		return 2
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	// openRuntime, not openRuntimeGC: sync reads the store and writes only
	// tree bytes — no store-write verb here, no identity GC pass owed.
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	return runSync(rt, repoTop, stdout, stderr)
}

// runSync is the orchestration: resolve integration (read-only, never
// bootstrap) → refuse if it's behind HEAD → enumerate divergence → decide →
// apply → report.
func runSync(rt *runtime, repoTop string, stdout, stderr io.Writer) int {
	integSHA, exists, err := syncIntegrationSHA(rt.Ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if !exists {
		// Neutral: nothing integrated yet is not a failure, and this must NOT
		// bootstrap the ref (gate.ResolveIntegrationRef would) — a repair verb
		// does not mint the authority it then enforces.
		fmt.Fprintln(stdout, "ℹ sync synced=0 conflicts=0 integration=absent")
		return 0
	}

	if code, refused := refuseIfBehindHead(rt.Ctx, repoTop, integSHA, stdout, stderr); refused {
		return code
	}

	entries, err := syncIntegrationEntries(rt.Ctx, repoTop, integSHA)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	diffs, err := syncDivergence(rt.Ctx, repoTop, entries)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(diffs) == 0 {
		fmt.Fprintln(stdout, "✓ sync synced=0 conflicts=0 tree=matches-integration")
		return 0
	}

	skipped, decidable := partitionSkipped(diffs)

	synced, conflicts, code := syncStoreDecideApply(rt, repoTop, integSHA, decidable, stderr)
	if code != 0 {
		// A mid-apply failure leaves the tree PARTIALLY fast-forwarded. Naming
		// the files that already changed is the whole report the operator gets
		// (loto-8sic); returning only the error would leave them to diff the
		// tree by hand to find out what moved.
		if len(synced) > 0 {
			fmt.Fprintf(stdout, "⚠ sync synced=%d partial=true — the tree is partially fast-forwarded\n", len(synced))
			for _, p := range synced {
				fmt.Fprintf(stdout, "✓ target=%s action=fast-forward\n", syncPathField(p))
			}
		}
		return code
	}

	emitSyncReport(stdout, synced, conflicts, skipped)
	if len(conflicts) > 0 {
		return 1
	}
	return 0
}

// syncStoreDecideApply reads live lock/claim/candidate-claim state, decides,
// and applies while the store holds the project operation flock. Every path
// that acquires one of those protections uses the same flock, so a peer cannot
// acquire after the decision and before the filesystem mutation.
// ‡ On a non-zero code the returned synced slice is still meaningful: apply is
// not atomic, so it names the files already published before the failure.
func syncStoreDecideApply(rt *runtime, repoTop, expectedIntegration string, decidable []syncDiff, stderr io.Writer) (synced []string, conflicts []syncConflict, code int) {
	paths := make([]string, len(decidable))
	for i, d := range decidable {
		paths[i] = d.Path
	}

	err := rt.Store.WithStableCoordinationState(rt.Ctx, paths, func(
		locks []domain.LockRecord,
		claims []domain.ClaimRecord,
		cands []domain.CandidateClaim,
	) error {
		currentIntegration, exists, err := syncIntegrationSHA(rt.Ctx, repoTop)
		if err != nil {
			return err
		}
		if !exists || currentIntegration != expectedIntegration {
			return fmt.Errorf("%w: expected=%s actual=%s", errSyncIntegrationChanged, expectedIntegration, currentIntegration)
		}

		ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe())}
		apply, decidedConflicts := syncDecide(decidable, locks, claims, cands, ec)
		conflicts = decidedConflicts
		if syncBeforeApplyFn != nil {
			syncBeforeApplyFn()
		}

		// ‡ syncApply returns what it published BEFORE it failed. Discarding
		// that on the error path leaves the caller unable to name which files
		// moved in a partially fast-forwarded tree.
		var applyErr error
		synced, applyErr = syncApply(rt.Ctx, repoTop, apply)
		return applyErr
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return synced, nil, 3
	}
	return synced, conflicts, 0
}

// partitionSkipped splits diffs into unsupported-mode rows (reported, never
// decided) and everything syncDecide must judge.
func partitionSkipped(diffs []syncDiff) (skipped, decidable []syncDiff) {
	for _, d := range diffs {
		if d.State == syncUnsupported {
			skipped = append(skipped, d)
		} else {
			decidable = append(decidable, d)
		}
	}
	return skipped, decidable
}

// refuseIfBehindHead is plan-review P1-2: refs/loto/integration existing and
// a strict ancestor of HEAD means main advanced past the frozen integration
// point (promotion hasn't shipped to advance the ref). Sync must not
// faithfully rewind newer committed content to the stale snapshot — it
// refuses before any divergence scan, let alone any write.
func refuseIfBehindHead(ctx context.Context, repoTop, integSHA string, stdout, stderr io.Writer) (code int, refused bool) {
	headSHA, err := syncHeadSHA(ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3, true
	}
	if integSHA == headSHA {
		return 0, false
	}
	behind, err := syncIsAncestor(ctx, repoTop, integSHA, headSHA)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3, true
	}
	if !behind {
		return 0, false
	}
	emitSyncBehindHead(stdout, integSHA, headSHA)
	return 1, true
}

// emitSyncBehindHead renders the P1-2 refusal: fixed triage line naming the
// reason, one ℹ row naming both SHAs, and a fix block pointing at what to
// inspect (no override flag exists in v1 — promotion is what resolves this).
func emitSyncBehindHead(w io.Writer, integSHA, headSHA string) {
	fmt.Fprintln(w, "✗ sync synced=0 conflicts=0 integration=behind-HEAD")
	fmt.Fprintf(w, "ℹ integration=%s head=%s\n", integSHA, headSHA)
	fmt.Fprintln(w, "```bash")
	fmt.Fprintf(w, "git log %s..%s --oneline   # commits sync would otherwise rewind unleased files past\n", integSHA, headSHA)
	fmt.Fprintln(w, "```")
}

// syncIntegrationSHA resolves refs/loto/integration to its current commit
// SHA, read-only. Deliberately NOT gate.ResolveIntegrationRef, which
// bootstraps the ref to HEAD as a side effect — sync is a repair verb; it
// must not mint the authority it then enforces.
func syncIntegrationSHA(ctx context.Context, repoTop string) (sha string, exists bool, err error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	//nolint:gosec // G204: the command is the literal "git"; args are internal plumbing tokens (a fixed ref-name constant), never shell-interpreted.
	cmd := exec.CommandContext(c, "git", "rev-parse", "--verify", "--quiet", gate.IntegrationRef)
	cmd.Dir = repoTop
	out, runErr := cmd.Output()
	if runErr != nil {
		if _, ok := errors.AsType[*exec.ExitError](runErr); ok {
			// --verify --quiet exits non-zero with no stderr when the ref is
			// simply absent — not an error, the expected fresh-repo shape.
			return "", false, nil
		}
		return "", false, fmt.Errorf("git rev-parse %s: %w", gate.IntegrationRef, runErr)
	}
	return strings.TrimSpace(string(out)), true, nil
}

// syncHeadSHA resolves HEAD's commit SHA.
func syncHeadSHA(ctx context.Context, repoTop string) (string, error) {
	out, err := gitOutput(ctx, repoTop, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(out), nil
}

// syncIsAncestor reports whether ancestor is a (non-strict) ancestor of
// descendant, per `git merge-base --is-ancestor`.
func syncIsAncestor(ctx context.Context, repoTop, ancestor, descendant string) (bool, error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, "git", "merge-base", "--is-ancestor", ancestor, descendant)
	cmd.Dir = repoTop
	err := cmd.Run()
	if err == nil {
		return true, nil
	}
	// Exit 1 means "not an ancestor" — the documented negative result, not a
	// failure. Any other exit code (128: bad object, etc.) is infra.
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("git merge-base --is-ancestor: %w", err)
}

// syncIntegrationEntries reads refs/loto/integration's full tree manifest —
// the shared checkout's index tracks HEAD, not integration, so this walks
// git's own object graph instead of the index (plan §2, git diff-index trap).
func syncIntegrationEntries(ctx context.Context, repoTop, sha string) ([]syncEntry, error) {
	out, err := gitOutput(ctx, repoTop, "ls-tree", "-r", "-z", sha)
	if err != nil {
		return nil, fmt.Errorf("git ls-tree %s: %w", sha, err)
	}
	var entries []syncEntry
	for rec := range strings.SplitSeq(strings.TrimRight(out, "\x00"), "\x00") {
		if rec == "" {
			continue
		}
		head, path, ok := strings.Cut(rec, "\t")
		if !ok {
			continue
		}
		fields := strings.Fields(head)
		if len(fields) != 3 {
			continue
		}
		entries = append(entries, syncEntry{Path: path, Mode: fields[0], OID: fields[2]})
	}
	return entries, nil
}

// syncDivergence partitions entries against the worktree: os.Lstat classifies
// each path as unsupported (dir/symlink/submodule where a blob is expected),
// missing, or an existing regular file — then one batched `git hash-object
// --stdin-paths -z` compares the existing files' content OID against
// integration's, with the lstat exec bit checked against the tree mode. No
// index involvement (plan §2).
func syncDivergence(ctx context.Context, repoTop string, entries []syncEntry) ([]syncDiff, error) {
	var diffs []syncDiff
	var existing []syncEntry
	for _, e := range entries {
		if e.Mode == "120000" || e.Mode == "160000" {
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncUnsupported})
			continue
		}
		fi, err := os.Lstat(filepath.Join(repoTop, filepath.FromSlash(e.Path)))
		switch {
		case os.IsNotExist(err):
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncMissing})
		case err != nil:
			return nil, fmt.Errorf("lstat %s: %w", e.Path, err)
		case fi.Mode()&os.ModeSymlink != 0 || fi.IsDir():
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncUnsupported})
		default:
			existing = append(existing, e)
		}
	}

	if len(existing) > 0 {
		sort.Slice(existing, func(i, j int) bool { return existing[i].Path < existing[j].Path })
		modeDiffs, err := compareExisting(ctx, repoTop, existing)
		if err != nil {
			return nil, err
		}
		diffs = append(diffs, modeDiffs...)
	}

	sort.Slice(diffs, func(i, j int) bool { return diffs[i].Path < diffs[j].Path })
	return diffs, nil
}

// compareExisting hashes every existing regular file in one batched
// `git hash-object` call and compares OID + exec bit against integration's
// recorded entry.
func compareExisting(ctx context.Context, repoTop string, existing []syncEntry) ([]syncDiff, error) {
	paths := make([]string, len(existing))
	for i, e := range existing {
		paths[i] = e.Path
	}
	oids, err := batchHashObject(ctx, repoTop, paths)
	if err != nil {
		return nil, err
	}
	var diffs []syncDiff
	for _, e := range existing {
		fi, err := os.Lstat(filepath.Join(repoTop, filepath.FromSlash(e.Path)))
		if err != nil {
			return nil, fmt.Errorf("lstat %s: %w", e.Path, err)
		}
		hasExecBit := fi.Mode()&0o111 != 0
		wantExec := e.Mode == "100755"
		switch {
		case oids[e.Path] != e.OID:
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncModified})
		case hasExecBit != wantExec:
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncModeOnly})
		}
	}
	return diffs, nil
}

// batchHashObject hashes ordinary paths in one newline-delimited
// `git hash-object --stdin-paths` call. Git offers no NUL-delimited form, so
// paths containing LF or CR are passed individually as argv after `--`.
func batchHashObject(ctx context.Context, repoTop string, paths []string) (map[string]string, error) {
	result := make(map[string]string, len(paths))
	var ordinary, exceptional []string
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\r") {
			exceptional = append(exceptional, p)
		} else {
			ordinary = append(ordinary, p)
		}
	}

	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	if err := hashOrdinaryPaths(c, repoTop, ordinary, result); err != nil {
		return nil, err
	}
	for _, p := range exceptional {
		oid, err := hashExceptionalPath(c, repoTop, p)
		if err != nil {
			return nil, err
		}
		result[p] = oid
	}
	return result, nil
}

func hashOrdinaryPaths(ctx context.Context, repoTop string, paths []string, result map[string]string) error {
	if len(paths) == 0 {
		return nil
	}
	cmd := exec.CommandContext(ctx, "git", "hash-object", "--stdin-paths")
	cmd.Dir = repoTop
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("git hash-object: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	oids := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(oids) != len(paths) {
		return fmt.Errorf("%w: got %d oids for %d paths", errHashObjectCountMismatch, len(oids), len(paths))
	}
	for i, p := range paths {
		result[p] = oids[i]
	}
	return nil
}

func hashExceptionalPath(ctx context.Context, repoTop, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "hash-object", "--", path)
	cmd.Dir = repoTop
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git hash-object %q: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	oids := strings.Fields(string(out))
	if len(oids) != 1 {
		return "", fmt.Errorf("%w: got %d oids for exceptional path %q", errHashObjectCountMismatch, len(oids), path)
	}
	return oids[0], nil
}

// syncDecide partitions divergent, decidable diffs into apply (safe to
// fast-forward) and conflicts (never written). Pure — no IO, clock only via
// ec.Now — φ gateDecide (cmd_check_gate.go). Deliberately has no myUUID
// parameter: unlike gateDecide/submitLeaseCheck, sync must not clobber ANY
// live lease or claim, including its own caller's (plan §3, plan-review P1-1).
func syncDecide(diffs []syncDiff, locks []domain.LockRecord, claims []domain.ClaimRecord, cands []domain.CandidateClaim, ec domain.EvalContext) (apply []syncDiff, conflicts []syncConflict) {
	locksByPath := map[string][]domain.LockRecord{}
	for i := range locks {
		l := &locks[i]
		locksByPath[l.Target.Canonical] = append(locksByPath[l.Target.Canonical], *l)
	}
	candsByPath := map[string][]domain.CandidateClaim{}
	for i := range cands {
		c := &cands[i]
		candsByPath[c.PathCanonical] = append(candsByPath[c.PathCanonical], *c)
	}

	for _, d := range diffs {
		if reason, holder, ok := syncConflictFor(d.Path, locksByPath[d.Path], claims, candsByPath[d.Path], ec); ok {
			conflicts = append(conflicts, syncConflict{Path: d.Path, Reason: reason, Holder: holder})
			continue
		}
		apply = append(apply, d)
	}

	sort.Slice(apply, func(i, j int) bool { return apply[i].Path < apply[j].Path })
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return apply, conflicts
}

// syncConflictFor checks, in order, whether path is covered by a live lease
// (any owner, beacons included), a live territory claim (no owner
// carve-out — ClaimCoversTarget called with an empty uuid per plan-review
// P1-1), or any candidate-claim row (liveness irrelevant — durable by
// design, a dead one is doctor's reclaim job, not sync's).
func syncConflictFor(path string, locks []domain.LockRecord, claims []domain.ClaimRecord, cands []domain.CandidateClaim, ec domain.EvalContext) (reason, holder string, conflict bool) {
	for i := range locks {
		l := &locks[i]
		if !ec.IsStale(*l) {
			return syncReasonLeased, string(l.OwnerUUID), true
		}
	}
	for i := range claims {
		c := &claims[i]
		if domain.ClaimCoversTarget(*c, path, "", ec.Now) && !ec.ClaimIsStale(*c) {
			return syncReasonTerritoryClaim, string(c.OwnerUUID), true
		}
	}
	if len(cands) > 0 {
		return syncReasonCandidateClaim, cands[0].CandidateID, true
	}
	return "", "", false
}

// syncApply fast-forwards every apply path to integration content, in sorted
// order: cat-file blob → MkdirAll (restores a path whose directory went with
// it) → atomic publish. Stops at the first failure. If publication succeeded
// before a later durability error, the current path is included in synced.
func syncApply(ctx context.Context, repoTop string, apply []syncDiff) (synced []string, err error) {
	sorted := append([]syncDiff(nil), apply...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, d := range sorted {
		published, applyErr := syncApplyOne(ctx, repoTop, d)
		if published {
			synced = append(synced, d.Path)
		}
		if applyErr != nil {
			return synced, fmt.Errorf("sync %s: %w", syncPathField(d.Path), applyErr)
		}
	}
	return synced, nil
}

func syncApplyOne(ctx context.Context, repoTop string, d syncDiff) (bool, error) {
	blob, err := gitOutputBytes(ctx, repoTop, "cat-file", "blob", d.OID)
	if err != nil {
		return false, fmt.Errorf("cat-file %s: %w", d.OID, err)
	}
	full := filepath.Join(repoTop, filepath.FromSlash(d.Path))
	if err := syncMkdirAll(filepath.Dir(full), 0o755); err != nil {
		return false, err
	}
	mode := os.FileMode(0o644)
	if d.Mode == "100755" {
		mode = 0o755
	}
	return syncAtomicReplace(full, blob, mode)
}

// syncMkdirAll is os.MkdirAll plus a durable fsync of every newly created
// level's parent, so a crash mid-repair can't leave a directory that
// MkdirAll reported as created but whose entry never reached disk — the
// same durability class the rename below closes (loto-8sic PR review,
// Codex). φ internal/identity's mkdirAllSync; duplicated rather than shared
// because identity is a leaf package (no internal imports) and this one
// routes through syncFull for the darwin F_FULLFSYNC barrier.
func syncMkdirAll(dir string, perm os.FileMode) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	// Levels that don't exist yet, deepest first, up to the first existing
	// ancestor (or the filesystem root).
	var created []string
	for p := dir; ; {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			break
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if err := os.MkdirAll(dir, perm); err != nil {
		return err
	}
	// Fsync top-down (shallowest first): a level's entry must be durable in
	// its parent before a crash could otherwise orphan it.
	for _, p := range slices.Backward(created) {
		if err := syncParentDirFn(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}

// syncAtomicReplace prepares the complete replacement beside path, including
// its final mode and a file fsync, before one rename publishes it. Failures
// before rename leave the old target intact; a parent-directory fsync failure
// after rename reports published=true so the partial-sync report names path.
func syncAtomicReplace(path string, data []byte, mode os.FileMode) (bool, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".loto-sync-*")
	if err != nil {
		return false, err
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	// The write seam targets only the unpublished temporary file: even an
	// injected short write can damage no existing worktree bytes.
	n, err := syncWriteFn(tmp, data)
	if err != nil {
		return false, err
	}
	if n != len(data) {
		return false, io.ErrShortWrite
	}
	if err := tmp.Chmod(mode); err != nil {
		return false, err
	}
	if err := syncFull(tmp); err != nil {
		return false, err
	}
	if err := tmp.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return false, err
	}
	if err := syncParentDirFn(dir); err != nil {
		return true, err
	}
	return true, nil
}

func syncParentDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFull(d)
}

// emitSyncReport renders the normal (non-special-cased) sync report:
// triage counts (skipped= always present per design.md determinism), then
// every row — synced, conflict, skipped — merged and sorted by path
// (byte-identical for the same input), then the fix block when there are
// conflicts to act on.
func emitSyncReport(w io.Writer, synced []string, conflicts []syncConflict, skipped []syncDiff) {
	glyph := "✓"
	if len(conflicts) > 0 {
		glyph = "✗"
	}
	fmt.Fprintf(w, "%s sync synced=%d conflicts=%d skipped=%d\n", glyph, len(synced), len(conflicts), len(skipped))

	rows := make([]syncReportRow, 0, len(synced)+len(conflicts)+len(skipped))
	for _, p := range synced {
		rows = append(rows, syncReportRow{path: p, line: fmt.Sprintf("✓ target=%s action=fast-forward", syncPathField(p))})
	}
	for _, c := range conflicts {
		rows = append(rows, syncReportRow{path: c.Path, line: formatSyncConflictRow(c)})
	}
	for _, d := range skipped {
		rows = append(rows, syncReportRow{path: d.Path, line: fmt.Sprintf("⚠ target=%s reason=unsupported-mode", syncPathField(d.Path))})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	for _, r := range rows {
		fmt.Fprintln(w, r.line)
	}

	if len(conflicts) > 0 {
		fmt.Fprintln(w, "```bash")
		fmt.Fprintln(w, "loto status --collisions   # see holders")
		fmt.Fprintln(w, "loto sync                  # re-run once the lease/claim resolves")
		fmt.Fprintln(w, "```")
	}
}

type syncReportRow struct {
	path string
	line string
}

// formatSyncConflictRow names the blocker: `holder=` (short uuid) for a
// leased or territory-claim row, `candidate=` (full candidate id) for a
// candidate-claim row — the two blocker kinds are addressed differently
// (unlock/wait vs. wait on candidate resolution).
func formatSyncConflictRow(c syncConflict) string {
	path := syncPathField(c.Path)
	if c.Reason == syncReasonCandidateClaim {
		return fmt.Sprintf("✗ target=%s reason=%s candidate=%s", path, c.Reason, c.Holder)
	}
	return fmt.Sprintf("✗ target=%s reason=%s holder=%s", path, c.Reason, shortUUID(c.Holder))
}

// syncPathField keeps one report row per path. Ordinary names retain the
// existing unquoted output; control characters use Go quoting so legal Git
// names containing a newline cannot inject a second row.
func syncPathField(path string) string {
	if strings.ContainsAny(path, "\n\r\t") {
		return strconv.Quote(path)
	}
	return path
}

// shortUUID truncates a UUID to its first 8 hex characters for compact
// display, φ render.holderTag's own uuid-prefix convention.
func shortUUID(uuid string) string {
	if len(uuid) > 8 {
		return uuid[:8]
	}
	return uuid
}

// gitOutput runs a git plumbing command in repoTop and returns trimmed-free
// stdout as a string, with stderr folded into the error on failure.
func gitOutput(ctx context.Context, repoTop string, args ...string) (string, error) {
	out, err := gitOutputBytes(ctx, repoTop, args...)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// gitOutputBytes runs a git plumbing command in repoTop and returns raw
// stdout bytes — cat-file blob content may be binary, so this is the shared
// primitive gitOutput wraps for text callers.
func gitOutputBytes(ctx context.Context, repoTop string, args ...string) ([]byte, error) {
	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(c, "git", args...)
	cmd.Dir = repoTop
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
