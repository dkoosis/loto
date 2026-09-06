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
	"loto/internal/store"
)

func init() { register("sync", cmdSync) } //nolint:gochecknoinits // command registry pattern

// syncUsageHead is the point-of-use teaching surface (loto-5rwc), φ submitUsageHead.
const syncUsageHead = `usage: loto sync [--dry-run] [--verbose]

Fast-forward every unleased path that diverges from refs/loto/integration to
integration content, delete the untracked residue a rejected candidate is on
record as having created, and report everything it refused to touch.

Divergence comes from rejected/abandoned candidates leaving tree residue,
out-of-band writes, and interrupted sessions — never ordinary promotion
(promoted blobs already originate in the tree). A path under a live lease, a
live territory claim, or an unresolved candidate claim is never written; it
is reported as a conflict row instead.

An untracked file is deleted ONLY when a rejected candidate's recorded
write-set names it as a path that candidate created AND the file still hashes
to the blob that candidate wrote there. A file whose bytes have changed since
is somebody's work now: it is counted as residue-modified and left alone.
Every other untracked file — build output, an editor dropping, a .env — is
counted as unattributed and left alone too; --verbose names both classes.
Attribution comes from the events table, so it reaches back only as far as
event retention: residue older than that window is unattributable and is never
deleted.

loto sync takes no positional arguments; run it from anywhere inside the repo.
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
//
// ‡ Observed is what the divergence scan hashed at that path — empty when
// nothing was there (syncMissing) or when the path is unsupported. It is the
// value the pre-write re-probe compares against, so the decision to write
// carries the evidence it was made on rather than trusting it to still hold
// (loto-gai7).
type syncDiff struct {
	syncEntry
	State    syncState
	Observed string
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
	// syncReasonResidueNotFile refuses a deletion whose target stopped being an
	// ordinary file between the scan and the delete — a directory or a symlink
	// now stands where the rejected candidate created a blob. Advisory (⚠), not
	// a conflict: nothing is wrong with the rest of the run, and the operator,
	// not sync, decides what that object is.
	syncReasonResidueNotFile = "residue-not-regular-file"
	// syncReasonResidueModified refuses a deletion whose target no longer holds
	// the bytes the rejected candidate wrote. Someone edited it, or re-created
	// the same path with different content — either way it is that person's
	// work now, not the rejection's residue.
	syncReasonResidueModified = "residue-modified"
	// syncReasonTargetModified refuses a fast-forward whose target stopped
	// being the file the divergence scan judged — different bytes, a
	// directory or symlink in its place, or gone. Whatever is there now
	// arrived after the decision to overwrite it was taken, so the decision
	// no longer covers it. Advisory (⚠): the next run re-classifies the path
	// against its current content.
	syncReasonTargetModified = "target-modified"
)

// syncResidue is one untracked worktree file a rejected candidate is on record
// as having CREATED — the only shape of file `loto sync` may delete
// (loto-ovno.13). CandidateID is the rejection the deletion is attributed to
// and appears on the row, so no byte leaves the tree unaccounted for
// (DESIGN.md invariant 8, "no silent dispossession"). Blob is the SHA that
// rejection recorded for the path; the file on disk must still hash to it.
type syncResidue struct {
	Path        string
	CandidateID string
	Blob        string
}

// syncResidueMismatch is attributed residue whose bytes have moved on: Found
// is what the worktree file hashes to now, against the recorded Blob.
type syncResidueMismatch struct {
	syncResidue
	Found string
}

// syncOpts carries the two flags that only change what sync is allowed to do.
type syncOpts struct {
	// DryRun makes the whole verb read-only — no fast-forward, no deletion.
	// The report names what it would have done, prefixed `would-`.
	DryRun bool
	// Verbose lists the untracked paths sync could not attribute (counted
	// either way; the count is what design.md's triage line owes the reader,
	// the list is what an operator hunting one file needs).
	Verbose bool
}

// syncTargetMismatch is a fast-forward the pre-write re-probe called off:
// Found is what the path hashes to now (empty when it vanished or stopped
// being an ordinary file), against syncDiff.Observed from the scan and
// syncDiff.OID, which is the content sync did not write.
type syncTargetMismatch struct {
	syncDiff
	Found string
}

// syncOutcome is what one decide+apply pass did, or (dry-run) would have done.
type syncOutcome struct {
	Synced       []string
	Deleted      []syncResidue
	Conflicts    []syncConflict
	ResidueSkips []syncConflict
	Modified     []syncResidueMismatch
	// TargetSkips are the apply paths that moved under the run — never
	// written, always reported.
	TargetSkips []syncTargetMismatch
}

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

// syncBeforeDeleteFn fires after residue classification and the fast-forward,
// immediately before the first deletion. Nil in production; tests use it to
// open the classify→delete window deterministically and prove a peer's write
// landing inside it is not deleted.
var syncBeforeDeleteFn func() //nolint:gochecknoglobals // production-nil concurrency test seam

// syncBeforeTargetProbeFn fires once per fast-forward path, after its
// replacement is staged and immediately before the probe that authorizes the
// rename — the window loto-gai7 closes, at its narrowest. Nil in production;
// tests use it to land a peer's bytes in that window for one named path.
var syncBeforeTargetProbeFn func(path string) //nolint:gochecknoglobals // production-nil concurrency test seam

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
	dryRun := fs.Bool("dry-run", false, "report what sync would fast-forward and delete; touch nothing")
	verbose := fs.Bool("verbose", false, "also list the untracked paths sync could not attribute")
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

	return runSync(rt, repoTop, syncOpts{DryRun: *dryRun, Verbose: *verbose}, stdout, stderr)
}

// runSync is the orchestration: resolve integration (read-only, never
// bootstrap) → refuse if it's behind HEAD → enumerate divergence → scan
// untracked residue and attribute it → decide → apply → report.
//
// ‡ Both refusal paths below return BEFORE the residue scan, deliberately. An
// absent integration ref means the gate has never run here, and a ref behind
// HEAD means sync's whole picture of the repo is stale — neither is a state in
// which this verb has earned the right to delete a file.
func runSync(rt *runtime, repoTop string, opts syncOpts, stdout, stderr io.Writer) int {
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

	residue, unattributed, err := syncScanResidue(rt, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	if len(diffs) == 0 && len(residue) == 0 && len(unattributed) == 0 {
		// The one shape that keeps v1's line: nothing diverges, nothing is
		// attributable, and no untracked file went uncounted — so every counter
		// the full report would print is zero and none is being hidden.
		fmt.Fprintln(stdout, "✓ sync synced=0 conflicts=0 tree=matches-integration")
		return 0
	}

	skipped, decidable := partitionSkipped(diffs)

	out, code := syncStoreDecideApply(rt, repoTop, integSHA, decidable, residue, opts, stderr)
	if code != 0 {
		emitSyncPartial(stdout, out)
		return code
	}

	emitSyncReport(stdout, out, skipped, unattributed, opts)
	if len(out.Conflicts) > 0 {
		return 1
	}
	return 0
}

// emitSyncPartial names what a failed run had already changed. A mid-apply
// failure leaves the tree PARTIALLY fast-forwarded (and possibly partially
// swept); naming those files is the whole report the operator gets
// (loto-8sic), and a deletion above all must never go unnamed.
func emitSyncPartial(w io.Writer, out syncOutcome) {
	if len(out.Synced) == 0 && len(out.Deleted) == 0 {
		return
	}
	fmt.Fprintf(w, "⚠ sync synced=%d deleted=%d partial=true — the tree is partially repaired\n",
		len(out.Synced), len(out.Deleted))
	for _, p := range out.Synced {
		fmt.Fprintf(w, "✓ target=%s action=fast-forward\n", syncPathField(p))
	}
	for _, r := range out.Deleted {
		fmt.Fprintf(w, "ℹ target=%s action=delete candidate=%s\n", syncPathField(r.Path), r.CandidateID)
	}
}

// syncScanResidue lists every untracked, non-ignored worktree file and splits
// it against the created-path record of the rejections the store still holds:
// attributed residue on one side, everything else on the other.
//
// ‡ The split is the safety property of this whole bead. Attribution is a
// positive fact a rejected candidate wrote down; absence of one is never
// evidence a file is disposable, so the unattributed side is COUNTED and left
// on disk — build output, a .env, an editor dropping and a residue file too
// old for event retention all land there together, which is correct, because
// sync cannot tell them apart and must not try.
func syncScanResidue(rt *runtime, repoTop string) (residue []syncResidue, unattributed []string, err error) {
	untracked, err := syncUntrackedPaths(rt.Ctx, repoTop)
	if err != nil {
		return nil, nil, err
	}
	if len(untracked) == 0 {
		return nil, nil, nil
	}
	createdBy, err := rt.Store.RejectedCandidateCreations(rt.Ctx)
	if err != nil {
		return nil, nil, err
	}
	for _, p := range untracked {
		if rc, ok := createdBy[p]; ok {
			residue = append(residue, syncResidue{Path: p, CandidateID: rc.CandidateID, Blob: rc.Blob})
			continue
		}
		unattributed = append(unattributed, p)
	}
	sort.Slice(residue, func(i, j int) bool { return residue[i].Path < residue[j].Path })
	sort.Strings(unattributed)
	return residue, unattributed, nil
}

// syncUntrackedPaths lists the worktree's untracked, non-ignored files as
// repo-relative slash paths.
//
// ‡ --exclude-standard stays: an ignored path is one the repo has already said
// is not its business, and a gate that can delete files is the wrong place to
// start second-guessing .gitignore. It costs nothing real — a rejected
// candidate's write-set names tracked-to-be source, not ignored output.
//
// -z because a Git path may contain a newline, and because git QUOTES such a
// path in the default output — the quoted form would then miss the exact-match
// against the recorded write-set and the file would go unattributed.
func syncUntrackedPaths(ctx context.Context, repoTop string) ([]string, error) {
	out, err := gitOutput(ctx, repoTop, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, fmt.Errorf("git ls-files --others: %w", err)
	}
	var paths []string
	for rec := range strings.SplitSeq(strings.TrimRight(out, "\x00"), "\x00") {
		if rec != "" {
			paths = append(paths, rec)
		}
	}
	return paths, nil
}

// syncStoreDecideApply reads live lock/claim/candidate-claim state, decides,
// and applies while the store holds the project operation flock. Every path
// that acquires one of those protections uses the same flock, so a peer cannot
// acquire after the decision and before the filesystem mutation.
// ‡ On a non-zero code the returned outcome is still meaningful: apply is not
// atomic, so it names the files already published or removed before the
// failure.
//
// ‡ Residue paths join the SAME stable-state read as the divergent ones, and
// are judged by the same syncConflictFor. A rejected candidate's created path
// can be under a peer's lease by now — the peer picked up the abandoned file
// and is working on it — and a deletion is the one repair with no undo, so it
// answers to every holder v1 already refuses to write over.
func syncStoreDecideApply(rt *runtime, repoTop, expectedIntegration string, decidable []syncDiff, residue []syncResidue, opts syncOpts, stderr io.Writer) (out syncOutcome, code int) {
	paths := make([]string, 0, len(decidable)+len(residue))
	for _, d := range decidable {
		paths = append(paths, d.Path)
	}
	for _, r := range residue {
		paths = append(paths, r.Path)
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

		ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), CaseFold: rt.CaseFold}
		apply, decidedConflicts := syncDecide(decidable, locks, claims, cands, ec)
		sweep, residueConflicts := syncDecideResidue(residue, locks, claims, cands, ec)
		out.Conflicts = mergeSyncConflicts(decidedConflicts, residueConflicts)
		if syncBeforeApplyFn != nil {
			syncBeforeApplyFn()
		}
		return syncApplyDecision(rt.Ctx, repoTop, apply, sweep, opts, &out)
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		out.Conflicts = nil
		return out, 3
	}
	return out, 0
}

// mergeSyncConflicts joins the divergence and residue refusals into one
// path-sorted list, so the report reads as a single verdict per path rather
// than as two passes the reader has to interleave.
func mergeSyncConflicts(a, b []syncConflict) []syncConflict {
	merged := make([]syncConflict, 0, len(a)+len(b))
	merged = append(merged, a...)
	merged = append(merged, b...)
	sort.Slice(merged, func(i, j int) bool { return merged[i].Path < merged[j].Path })
	return merged
}

// syncApplyDecision carries out (or, under --dry-run, only records) the
// decision: fast-forward first, then the sweep. Order is deliberate — a
// deletion is the irreversible half, so it runs last, after the reversible
// half has proved the tree is writable.
//
// --dry-run performs neither half. The decision is what an operator is
// checking before letting sync delete anything, so it is still computed
// against the same live state a real run would use, under the same flock.
//
// ‡ Both appliers return what they completed BEFORE failing, and both results
// are recorded on out even on the error path: a caller that cannot name the
// files that moved in a partially repaired tree has no report to give.
// ‡ The content check runs in BOTH modes, before either branch. --dry-run has
// to answer "would this delete my file?", and it can only answer honestly if
// it applies every test the real run does.
func syncApplyDecision(ctx context.Context, repoTop string, apply []syncDiff, sweep []syncResidue, opts syncOpts, out *syncOutcome) error {
	deletable, skips, modified, classifyErr := syncClassifyResidue(ctx, repoTop, sweep)
	if classifyErr != nil {
		return classifyErr
	}
	out.ResidueSkips, out.Modified = skips, modified

	if opts.DryRun {
		for _, d := range apply {
			out.Synced = append(out.Synced, d.Path)
		}
		out.Deleted = deletable
		return nil
	}
	var applyErr error
	out.Synced, out.TargetSkips, applyErr = syncApply(ctx, repoTop, apply)
	if applyErr != nil {
		return applyErr
	}
	if syncBeforeDeleteFn != nil {
		syncBeforeDeleteFn()
	}
	deleted, lateSkips, lateModified, deleteErr := syncDeleteResidue(ctx, repoTop, deletable)
	out.Deleted = deleted
	out.ResidueSkips = append(out.ResidueSkips, lateSkips...)
	out.Modified = append(out.Modified, lateModified...)
	sort.Slice(out.ResidueSkips, func(i, j int) bool { return out.ResidueSkips[i].Path < out.ResidueSkips[j].Path })
	sort.Slice(out.Modified, func(i, j int) bool { return out.Modified[i].Path < out.Modified[j].Path })
	return deleteErr
}

// syncProbeState is what one probe of a worktree path found.
type syncProbeState int

const (
	// syncProbeHashed: an ordinary file, and the probe's oid is its current
	// content. Whether that content is the one the caller expected is the
	// CALLER's comparison — the probe reports what is there, it does not
	// judge it.
	syncProbeHashed syncProbeState = iota
	// syncProbeVanished: nothing at the path any more. Someone got there
	// first.
	syncProbeVanished
	// syncProbeNotRegular: a directory or a symlink now stands there.
	syncProbeNotRegular
)

// syncProbePath answers, for one path, the only question that authorizes
// touching it: is this still an ordinary file, and what does it hold RIGHT
// NOW? Both halves of sync ask immediately before they act — the deletion
// half against the blob a rejected candidate wrote, the fast-forward half
// against the oid the divergence scan hashed.
//
// ‡ Lstat FIRST, then hash, and both per file. Ordering: `git hash-object`
// follows a symlink, so hashing first reads a live link's target and fails
// outright on a dangling one. Granularity: a batched
// `git hash-object --stdin-paths` exits 128 and takes the WHOLE sync down when
// any one of its paths has vanished since the scan — a peer cleaning up its
// own residue mid-run should cost that one path, not the fast-forward of every
// other file in the tree. One process per probed file is affordable because
// both callers probe a bounded set — what a rejection declared, or what the
// scan already found divergent — unlike the divergence scan's whole-tree
// manifest, which keeps the batch.
//
// A hash failure is re-probed rather than trusted: if the file is gone by
// then, the failure WAS the race and the path is simply vanished.
func syncProbePath(ctx context.Context, repoTop, path string) (oid string, state syncProbeState, err error) {
	full := filepath.Join(repoTop, filepath.FromSlash(path))
	fi, statErr := os.Lstat(full)
	switch {
	case os.IsNotExist(statErr):
		return "", syncProbeVanished, nil
	case statErr != nil:
		return "", syncProbeVanished, fmt.Errorf("lstat %s: %w", syncPathField(path), statErr)
	case !fi.Mode().IsRegular():
		return "", syncProbeNotRegular, nil
	}

	c, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	oid, hashErr := hashExceptionalPath(c, repoTop, path)
	if hashErr != nil {
		if _, again := os.Lstat(full); os.IsNotExist(again) {
			return "", syncProbeVanished, nil
		}
		return "", syncProbeVanished, hashErr
	}
	return oid, syncProbeHashed, nil
}

// syncClassifyResidue decides which attributed residue is still the rejected
// candidate's own bytes, and is therefore deletable. See syncProbePath for the
// two gates and why they run in that order, per file.
//
// ‡ The content gate is the review's correction, and the asymmetry behind it
// is worth stating: a fast-forward RESTORES a known-good state, so being wrong
// about it costs an undo; a deletion restores nothing, so being wrong about it
// costs the bytes. Same path, different content means a peer re-created that
// file with their own work — the path matched, the file did not.
func syncClassifyResidue(ctx context.Context, repoTop string, sweep []syncResidue) (deletable []syncResidue, skips []syncConflict, modified []syncResidueMismatch, err error) {
	for _, r := range sweep {
		oid, state, probeErr := syncProbePath(ctx, repoTop, r.Path)
		if probeErr != nil {
			return nil, nil, nil, probeErr
		}
		switch {
		case state == syncProbeVanished:
			continue
		case state == syncProbeNotRegular:
			skips = append(skips, syncConflict{Path: r.Path, Reason: syncReasonResidueNotFile, Holder: r.CandidateID})
		case oid == r.Blob:
			deletable = append(deletable, r)
		default:
			modified = append(modified, syncResidueMismatch{syncResidue: r, Found: oid})
		}
	}
	sort.Slice(modified, func(i, j int) bool { return modified[i].Path < modified[j].Path })
	return deletable, skips, modified, nil
}

// syncDecideResidue partitions attributed residue into what may be deleted and
// what a live holder blocks, using the same predicate as syncDecide. Pure.
func syncDecideResidue(residue []syncResidue, locks []domain.LockRecord, claims []domain.ClaimRecord, cands []domain.CandidateClaim, ec domain.EvalContext) (sweep []syncResidue, conflicts []syncConflict) {
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
	for _, r := range residue {
		if reason, holder, ok := syncConflictFor(r.Path, locksByPath[r.Path], claims, candsByPath[r.Path], ec); ok {
			conflicts = append(conflicts, syncConflict{Path: r.Path, Reason: reason, Holder: holder})
			continue
		}
		sweep = append(sweep, r)
	}
	sort.Slice(sweep, func(i, j int) bool { return sweep[i].Path < sweep[j].Path })
	sort.Slice(conflicts, func(i, j int) bool { return conflicts[i].Path < conflicts[j].Path })
	return sweep, conflicts
}

// syncDeleteResidue removes the classified-deletable residue, in sorted order,
// stopping at the first failure and returning what it had already removed.
//
// ‡ The FULL probe runs again here, immediately before each os.Remove — not
// just the Lstat. Classification walks every residue path and the
// fast-forward runs between, so there is a real window in which a peer can
// overwrite an attributed file with its own work; re-checking only the file
// TYPE would still delete those bytes. A path that changed in that window is
// reported exactly as it would have been had classification caught it.
//
// os.Remove, never RemoveAll: a directory is never a write-set path, and the
// recursive form is a mistake this function must not be able to make.
func syncDeleteResidue(ctx context.Context, repoTop string, deletable []syncResidue) (deleted []syncResidue, skips []syncConflict, modified []syncResidueMismatch, err error) {
	for _, r := range deletable {
		oid, state, probeErr := syncProbePath(ctx, repoTop, r.Path)
		if probeErr != nil {
			return deleted, skips, modified, probeErr
		}
		switch {
		case state == syncProbeVanished:
			continue
		case state == syncProbeNotRegular:
			skips = append(skips, syncConflict{Path: r.Path, Reason: syncReasonResidueNotFile, Holder: r.CandidateID})
			continue
		case oid != r.Blob:
			modified = append(modified, syncResidueMismatch{syncResidue: r, Found: oid})
			continue
		}
		if rmErr := os.Remove(filepath.Join(repoTop, filepath.FromSlash(r.Path))); rmErr != nil {
			return deleted, skips, modified, fmt.Errorf("delete %s: %w", syncPathField(r.Path), rmErr)
		}
		deleted = append(deleted, r)
	}
	return deleted, skips, modified, nil
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
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncModified, Observed: oids[e.Path]})
		case hasExecBit != wantExec:
			diffs = append(diffs, syncDiff{syncEntry: e, State: syncModeOnly, Observed: oids[e.Path]})
		}
	}
	return diffs, nil
}

// batchHashObject hashes ordinary paths in one newline-delimited
// `git hash-object --stdin-paths` call. Git offers no NUL-delimited form, so
// a path its line grammar cannot carry goes through argv after `--` instead
// — see syncNeedsArgvPath.
func batchHashObject(ctx context.Context, repoTop string, paths []string) (map[string]string, error) {
	result := make(map[string]string, len(paths))
	var ordinary, exceptional []string
	for _, p := range paths {
		if syncNeedsArgvPath(p) {
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

// syncNeedsArgvPath reports whether a path must bypass
// `git hash-object --stdin-paths`.
//
// ‡ LF and CR end a line, so such a path cannot be fed through the stream at
// all. A path STARTING with a double quote is the worse case: git
// C-style-unquotes any line that begins with one, so a file literally named
// `"a.txt` aborts the WHOLE batch — `fatal: line is badly quoted`, exit 128,
// every sync run in the repo down (loto-g870) — and a validly quoted spelling
// like `"a.txt"` unquotes to a DIFFERENT path, whose oid is then paired
// positionally with the name that was fed in. Backslash rides along because it
// is the escape character that grammar consumes; routing it costs one extra
// fork per such path and removes the need to reason about the interaction.
func syncNeedsArgvPath(p string) bool {
	return strings.ContainsAny(p, "\n\r\\") || strings.HasPrefix(p, `"`)
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
		if ec.ClaimCovers(*c, path, "") && !ec.ClaimIsStale(*c) {
			return syncReasonTerritoryClaim, string(c.OwnerUUID), true
		}
	}
	if len(cands) > 0 {
		return syncReasonCandidateClaim, cands[0].CandidateID, true
	}
	return "", "", false
}

// syncApply fast-forwards every apply path to integration content, in sorted
// order: stage the replacement (cat-file blob → MkdirAll, which restores a path
// whose directory went with it → write+fsync a temporary beside the target) →
// probe → rename. Stops at the first failure. If publication succeeded before a
// later durability error, the current path is included in synced.
//
// ‡ The probe runs again here, immediately before each write — φ
// syncDeleteResidue, and for the same reason (loto-gai7). The divergence scan
// hashes the whole manifest, the decision runs against coordination state, and
// the residue classification runs between; a peer holding no lease can land its
// own bytes in that window, and overwriting them would be a silent
// dispossession the report calls action=fast-forward. A path that moved is
// skipped and named, never written.
//
// ‡ Everything expensive is staged BEFORE the probe so that nothing but the
// rename follows it — φ syncDeleteResidue's probe → os.Remove. Reading the
// blob, creating parent directories and fsyncing a temporary file are each
// slower than the write they precede, and every one of them left inside the
// window would widen the very race the probe closes. Staging touches no
// worktree path the report names; a skip discards the temporary and leaves the
// peer's bytes where they are.
func syncApply(ctx context.Context, repoTop string, apply []syncDiff) (synced []string, skipped []syncTargetMismatch, err error) {
	sorted := append([]syncDiff(nil), apply...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	for _, d := range sorted {
		staged, stageErr := syncStageReplacement(ctx, repoTop, d)
		if stageErr != nil {
			return synced, skipped, fmt.Errorf("sync %s: %w", syncPathField(d.Path), stageErr)
		}
		if syncBeforeTargetProbeFn != nil {
			syncBeforeTargetProbeFn(d.Path)
		}
		oid, state, probeErr := syncProbePath(ctx, repoTop, d.Path)
		if probeErr != nil {
			syncDiscardStaged(staged)
			return synced, skipped, probeErr
		}
		if found, moved := syncTargetMoved(d, oid, state); moved {
			syncDiscardStaged(staged)
			skipped = append(skipped, syncTargetMismatch{syncDiff: d, Found: found})
			continue
		}
		published, pubErr := syncPublishStaged(staged)
		if published {
			synced = append(synced, d.Path)
		} else {
			syncDiscardStaged(staged)
		}
		if pubErr != nil {
			return synced, skipped, fmt.Errorf("sync %s: %w", syncPathField(d.Path), pubErr)
		}
	}
	return synced, skipped, nil
}

// syncTargetMoved compares one fresh probe against what classification saw and
// reports whether the write is still the one that was decided. Pure.
//
// A syncMissing path was decided on the absence of a file, so anything now
// standing there — bytes or a directory — cancels it. Every other state was
// decided on d.Observed, the oid the scan hashed, so the write survives only an
// ordinary file still holding exactly that. Vanished counts as moved rather
// than as a free restore: the file was removed after the decision, and the next
// run reclassifies it as missing and restores it then, with that removal in
// evidence.
func syncTargetMoved(d syncDiff, oid string, state syncProbeState) (found string, moved bool) {
	if d.State == syncMissing {
		return oid, state != syncProbeVanished
	}
	if state != syncProbeHashed {
		return "", true
	}
	return oid, oid != d.Observed
}

// syncStaged is one path's complete replacement, written and fsynced beside
// the target and waiting for the single rename that publishes it. Nothing at
// the target has been touched while it exists, so discarding it is always
// safe.
type syncStaged struct {
	TmpPath string
	Target  string
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

// syncStageReplacement builds one path's replacement in full — integration's
// blob, the parent directories it needs, the final mode, a file fsync — and
// stops one rename short of publishing it. Every failure here leaves the
// worktree file untouched, which is what lets the probe run last.
func syncStageReplacement(ctx context.Context, repoTop string, d syncDiff) (syncStaged, error) {
	blob, err := gitOutputBytes(ctx, repoTop, "cat-file", "blob", d.OID)
	if err != nil {
		return syncStaged{}, fmt.Errorf("cat-file %s: %w", d.OID, err)
	}
	full := filepath.Join(repoTop, filepath.FromSlash(d.Path))
	if err := syncMkdirAll(filepath.Dir(full), 0o755); err != nil {
		return syncStaged{}, err
	}
	mode := os.FileMode(0o644)
	if d.Mode == "100755" {
		mode = 0o755
	}
	tmp, err := os.CreateTemp(filepath.Dir(full), ".loto-sync-*")
	if err != nil {
		return syncStaged{}, err
	}
	staged := syncStaged{TmpPath: tmp.Name(), Target: full}
	if err := syncFillStaged(tmp, blob, mode); err != nil {
		_ = tmp.Close()
		syncDiscardStaged(staged)
		return syncStaged{}, err
	}
	return staged, nil
}

// syncFillStaged writes the temporary file and leaves it closed, durable and
// wearing its final mode.
//
// ‡ The write seam targets only the unpublished temporary file: even an
// injected short write can damage no existing worktree bytes.
func syncFillStaged(tmp *os.File, data []byte, mode os.FileMode) error {
	n, err := syncWriteFn(tmp, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	if err := tmp.Chmod(mode); err != nil {
		return err
	}
	if err := syncFull(tmp); err != nil {
		return err
	}
	return tmp.Close()
}

// syncPublishStaged is the rename that makes the staged content the target,
// plus the parent-directory fsync that makes the rename durable. A fsync
// failure after the rename reports published=true so the partial-sync report
// names the path.
func syncPublishStaged(s syncStaged) (bool, error) {
	if err := os.Rename(s.TmpPath, s.Target); err != nil {
		return false, err
	}
	if err := syncParentDirFn(filepath.Dir(s.Target)); err != nil {
		return true, err
	}
	return true, nil
}

// syncDiscardStaged drops an unpublished replacement. The target is whatever
// it was; only the temporary goes.
func syncDiscardStaged(s syncStaged) {
	if s.TmpPath != "" {
		_ = os.Remove(s.TmpPath)
	}
}

func syncParentDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return syncFull(d)
}

// emitSyncReport renders the normal (non-special-cased) sync report: triage
// counts (every counter always present per design.md determinism), the
// attribution-window row, then every row — synced, deleted, conflict,
// skipped — merged and sorted by path (byte-identical for the same input),
// then the fix block when there are conflicts to act on.
//
// ‡ deleted=, residue-modified= and unattributed= are on line 1
// unconditionally. A deletion that only shows up in the row list is a deletion
// a reader who greps the first line can miss, and DESIGN.md invariant 8
// forbids bytes leaving quietly. Together they read as the full account of
// untracked matter: what sync removed, what it was told about but found
// changed, and what nothing vouched for at all.
//
// --dry-run relabels rather than reusing the past tense: synced= would be a
// lie about a run that wrote nothing, so the counters read would-fast-forward=
// / would-delete= and the rows read action=would-*.
func emitSyncReport(w io.Writer, out syncOutcome, skipped []syncDiff, unattributed []string, opts syncOpts) {
	glyph := "✓"
	if len(out.Conflicts) > 0 {
		glyph = "✗"
	}
	// ‡ A target the pre-write re-probe called off counts as skipped=, beside
	// the unsupported-mode and residue rows: three ways of not acting on a
	// path, one counter, one ⚠ row each naming its own reason.
	skippedN := len(skipped) + len(out.ResidueSkips) + len(out.TargetSkips)
	if opts.DryRun {
		fmt.Fprintf(w, "ℹ sync dry-run=true would-fast-forward=%d conflicts=%d skipped=%d would-delete=%d residue-modified=%d unattributed=%d\n",
			len(out.Synced), len(out.Conflicts), skippedN, len(out.Deleted), len(out.Modified), len(unattributed))
	} else {
		fmt.Fprintf(w, "%s sync synced=%d conflicts=%d skipped=%d deleted=%d residue-modified=%d unattributed=%d\n",
			glyph, len(out.Synced), len(out.Conflicts), skippedN, len(out.Deleted), len(out.Modified), len(unattributed))
	}
	fmt.Fprintf(w, "ℹ attribution=rejected-candidate-write-set+blob window=%s max-events=%d\n",
		syncRetentionWindow(), store.EventsRetentionMaxRows)

	for _, r := range syncReportRows(out, skipped, unattributed, opts) {
		fmt.Fprintln(w, r.line)
	}

	emitSyncFixBlock(w, out, unattributed, opts)
}

// syncReportRows builds every per-path row — synced, deleted, conflict,
// skipped, and (under --verbose) the two left-alone classes — and sorts them
// by path, so the same outcome always renders byte-identically.
func syncReportRows(out syncOutcome, skipped []syncDiff, unattributed []string, opts syncOpts) []syncReportRow {
	rows := make([]syncReportRow, 0, len(out.Synced)+len(out.Deleted)+len(out.Conflicts)+
		len(out.ResidueSkips)+len(out.TargetSkips)+len(skipped)+len(out.Modified)+len(unattributed))
	ffAction, delAction := "fast-forward", "delete"
	if opts.DryRun {
		ffAction, delAction = "would-fast-forward", "would-delete"
	}
	for _, p := range out.Synced {
		rows = append(rows, syncReportRow{path: p, line: fmt.Sprintf("✓ target=%s action=%s", syncPathField(p), ffAction)})
	}
	for _, r := range out.Deleted {
		rows = append(rows, syncReportRow{path: r.Path, line: fmt.Sprintf("ℹ target=%s action=%s candidate=%s", syncPathField(r.Path), delAction, r.CandidateID)})
	}
	for _, c := range out.Conflicts {
		rows = append(rows, syncReportRow{path: c.Path, line: formatSyncConflictRow(c)})
	}
	for _, c := range out.ResidueSkips {
		rows = append(rows, syncReportRow{path: c.Path, line: fmt.Sprintf("⚠ target=%s reason=%s candidate=%s", syncPathField(c.Path), c.Reason, c.Holder)})
	}
	for _, d := range skipped {
		rows = append(rows, syncReportRow{path: d.Path, line: fmt.Sprintf("⚠ target=%s reason=unsupported-mode", syncPathField(d.Path))})
	}
	for _, m := range out.TargetSkips {
		rows = append(rows, syncReportRow{path: m.Path, line: fmt.Sprintf(
			"⚠ target=%s reason=%s found=%s want=%s",
			syncPathField(m.Path), syncReasonTargetModified, shortOID(m.Found), shortOID(m.OID))})
	}
	if opts.Verbose {
		// Both --verbose classes are files sync deliberately did not touch. The
		// counts carry them on line 1 either way; the rows are for the operator
		// hunting one specific file and asking why it is still there.
		for _, m := range out.Modified {
			rows = append(rows, syncReportRow{path: m.Path, line: fmt.Sprintf(
				"⚠ target=%s reason=%s candidate=%s recorded=%s found=%s",
				syncPathField(m.Path), syncReasonResidueModified, m.CandidateID, shortOID(m.Blob), shortOID(m.Found))})
		}
		for _, p := range unattributed {
			rows = append(rows, syncReportRow{path: p, line: fmt.Sprintf("ℹ target=%s reason=unattributed action=none", syncPathField(p))})
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].path < rows[j].path })
	return rows
}

// emitSyncFixBlock prints the one actionable next step per finding class, per
// design.md's inline-fix rule: how to see a conflict's holder, and how to see
// the untracked files sync deliberately did not touch.
func emitSyncFixBlock(w io.Writer, out syncOutcome, unattributed []string, opts syncOpts) {
	showLeftAlone := (len(unattributed) > 0 || len(out.Modified) > 0) && !opts.Verbose
	if len(out.Conflicts) == 0 && len(out.TargetSkips) == 0 && !showLeftAlone {
		return
	}
	fmt.Fprintln(w, "```bash")
	if len(out.Conflicts) > 0 {
		fmt.Fprintln(w, "loto status --collisions   # see holders")
		fmt.Fprintln(w, "loto sync                  # re-run once the lease/claim resolves")
	}
	if len(out.TargetSkips) > 0 {
		// The bytes on disk are safe — what a target-modified row's reader
		// needs is the content sync declined to write over them.
		fmt.Fprintln(w, "git cat-file blob <want>   # integration's bytes for a target-modified row")
	}
	if showLeftAlone {
		fmt.Fprintln(w, "loto sync --verbose        # name the untracked files sync left alone")
	}
	fmt.Fprintln(w, "```")
}

// shortOID truncates a git object id for display. φ shortUUID's own
// prefix convention; kept separate because these are blob SHAs, not agent
// identities, and one day the two may want different widths.
func shortOID(oid string) string {
	if len(oid) > 8 {
		return oid[:8]
	}
	if oid == "" {
		return "none"
	}
	return oid
}

// syncRetentionWindow renders the events-retention age as whole days when it
// divides evenly, so the report says 7d rather than 168h0m0s.
func syncRetentionWindow() string {
	const day = 24 * time.Hour
	if store.EventsRetentionAge%day == 0 {
		return strconv.FormatInt(int64(store.EventsRetentionAge/day), 10) + "d"
	}
	return store.EventsRetentionAge.String()
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
