//go:build unix

// Family A — cross-process TOCTOU (loto-qhv build-order item 3). Builds on
// the harness spine landed in crossproc_main_test.go (#236): TestMain role
// dispatch, spawnChild, the pipe barrier, the JSON verdict, process-group
// cleanup, the child watchdog, and the acquireOpFlockFn seam. Its role
// functions are registered as rows in crossProcRoles (declared in
// crossproc_main_test.go) rather than opening a second TestMain. See the
// loto-qhv plan
// (~/Projects/dk/Project/loto/plans/loto-qhv-crossproc-contract-tests.md) §3
// Family A for the full design.
//
// A1 is the cross-process twin of TestConcurrentOverlappingAcquire
// (concurrent_test.go): N processes race AcquireLocks on the same target
// set; exactly one wins.
//
// A2 is loto-7wp.6's shape (b), which never landed: a real holder dies, gets
// SIGKILLed and reaped BEFORE any reclaimer starts (so "the holder is dead"
// is a fact, not a race), then N processes race the stale reclaim.
//
// A2-control, as literally specified in the plan (a bare `fault=no-flock`
// rerun of the four-way single-target A2 race), is NOT implemented — it is
// unreachable, with or without the flock:
//   - every cross-process AcquireLocks call is already fully serialized by
//     SQLite's `_txlock=immediate` (plan §1.1) — a second writer's beginTx
//     cannot even start until the first commits, so two reclaimers never
//     touch the filesystem concurrently for the same target;
//   - a winner's own reclaimed-and-restripped target is always skipped by
//     restoreReclaimedSkippingRestripped's restripped-guard (unconditional,
//     independent of the flock) — restoreWrite is only ever reachable for a
//     target reclaimed at SHARED mode;
//   - any peer that could exploit a shared-then-restore window by acquiring
//     the SAME target EXCLUSIVE is itself blocked by EvalContext.Conflicts
//     against a fresh, already-consistent DB read — independent of the
//     flock.
//
// Building the literal control would be either always-failing (misreporting
// a real invariant as broken) or vacuous (plan §6, "worse than none").
//
// What IS implemented, as testCrossProcA2ControlNoFlock (t.Run
// "control/fault=no-flock" under TestCrossProc_StaleReclaimSingleWinner): a
// two-actor choreography that reaches the flock's actual load-bearing job
// (plan §1.1 item 4). S reclaims H's real-dead stale EXCLUSIVE row while
// acquiring the SAME target SHARED — stripAll skips a shared acquire, so
// restoreReclaimedSkippingRestripped's post-commit restore genuinely fires.
// S's own row is deliberately backdated (ExpiresAt already elapsed at
// insert), mirroring TestAcquireLocks_HoldsFlockAcrossRestore's in-process
// precedent (locks_acquire_flock_test.go:31): it makes S's own freshly
// committed row immediately TTL-stale too, so E can reclaim past it without
// ever needing S to look dead. E then acquires EXCLUSIVE on the same
// target. With acquireOpFlockFn neutered in both children
// (LOTO_CROSSPROC_FAULT=no-flock), nothing stops E's whole acquire — strip,
// insert, commit — from landing strictly between S's commit and S's
// restore, so S's lagging restore silently re-adds owner-write under E's
// now-live exclusive row.
//
// Proof the flock is genuinely load-bearing here, on the SUCCESS path (the
// question this control's validity turns on): locks_acquire.go:45 acquires
// the flock and line 49 defers its release as the failure-path backstop
// only; the success path (line 134) calls restoreThenReleaseFlock, whose
// body (locks_acquire.go:172-174) runs restoreReclaimedSkippingRestripped —
// the real chmod restore — BEFORE flock.release(). So production holds the
// flock across the restore; only the fault removes that.
//
// Verified (not shipped, per plan §6 "vacuous controls" discipline) by
// running the identical S/E choreography WITHOUT the fault: E's own
// acquireOpFlockFn call blocks on S's still-held real flock (S holds it
// until its restore completes, same lines) and times out rather than ever
// landing an exclusive acquire, so E cannot win and the asserted state
// (owner-writable target + live exclusive row) cannot arise. Red here is
// attributable to the fault, not to the choreography.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"loto/internal/domain"
)

const (
	crossProcRoleAcquireName               = "acquire"
	crossProcRoleHoldName                  = "hold"
	crossProcRoleReclaimAcquireName        = "reclaim-acquire"
	crossProcRoleReclaimSharedFaultName    = "reclaim-shared-fault"
	crossProcRoleReclaimExclusiveFaultName = "reclaim-exclusive-fault"
)

// Role functions (crossProcRoleAcquire, crossProcRoleHold,
// crossProcRoleReclaimAcquire, crossProcRoleReclaimSharedFault,
// crossProcRoleReclaimExclusiveFault) are registered as rows in
// crossProcRoles — the shared dispatch table declared in
// crossproc_main_test.go — rather than via an init() here (gochecknoinits,
// make lint).

const (
	envDBPath  = "LOTO_CROSSPROC_DB"
	envOwner   = "LOTO_CROSSPROC_OWNER"
	envTargets = "LOTO_CROSSPROC_TARGETS" // A1: comma-separated, per-child rotated order
	envTarget  = "LOTO_CROSSPROC_TARGET"  // A2 + A2-control: single target
	envFault   = "LOTO_CROSSPROC_FAULT"   // A2-control only: "no-flock"
)

// crossProcFaultNoFlock gates maybeInjectNoFlockFault — the ONE fault this
// file injects, and only inside the two A2-control roles' own child
// process. Production's acquireOpFlockFn default is never touched.
const crossProcFaultNoFlock = "no-flock"

// maybeInjectNoFlockFault swaps this process's own acquireOpFlockFn to a
// no-op handle when LOTO_CROSSPROC_FAULT=no-flock — the production seam
// PR #236 (loto-qhv items 1-2) added to flock.go, exercised here rather
// than in production.
func maybeInjectNoFlockFault() {
	if os.Getenv(envFault) == crossProcFaultNoFlock {
		acquireOpFlockFn = func(context.Context, string, io.Writer) (*opFlock, error) {
			return &opFlock{}, nil // f is nil: opFlock.release() is a no-op on it (flock.go).
		}
	}
}

// crossProcA2Host is the Host every A2 actor (H and the reclaimers) stamps
// on its LockRecord and every A2 probe evaluates liveness from — matching
// hostPidProbe's "which machine am I asking from" contract (liveprobe_test.go).
const crossProcA2Host = "crossproc-a2"

// crossProcRealPidProbe consults REAL pid liveness via syscall.Kill(pid, 0)
// directly — internal/store cannot import internal/identity (arch-lint), so
// this is the store-side twin of what the CLI's liveness oracle does in
// production. Reuses the existing hostPidProbe adapter (liveprobe_test.go).
func crossProcRealPidProbe(host string) domain.HolderLiveProbe {
	return hostPidProbe(host, func(pid int, _ int64) bool {
		return syscall.Kill(pid, 0) == nil
	})
}

// crossProcAwaitBarrierAt is crossProcAwaitBarrier's fd-parameterized twin:
// signal ready on readyFd, then block reading startFd until the far end
// closes. crossProcAwaitBarrier (crossproc_main_test.go) hardcodes fd 3/4
// for the one race-start barrier every role uses; A2's reclaimers need a
// SECOND, independent barrier (fd 5/6) to hold themselves alive after
// computing a verdict — see crossProcRoleReclaimAcquire for why.
func crossProcAwaitBarrierAt(startFd, readyFd int) error {
	readyW := os.NewFile(uintptr(readyFd), "crossproc-ready")
	if _, err := readyW.Write([]byte{1}); err != nil {
		return fmt.Errorf("signal ready (fd %d): %w", readyFd, err)
	}
	if err := readyW.Close(); err != nil {
		return fmt.Errorf("close ready fd %d: %w", readyFd, err)
	}
	startR := os.NewFile(uintptr(startFd), "crossproc-start")
	defer startR.Close()
	buf := make([]byte, 1)
	if _, err := startR.Read(buf); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("await start (fd %d): %w", startFd, err)
	}
	return nil
}

// errCrossProcPathNotValidated is the sentinel behind every env-sourced
// path rejection below (err113: no dynamic fmt.Errorf without a wrapped
// sentinel).
var errCrossProcPathNotValidated = errors.New("crossproc: env-sourced path is not an absolute, clean path")

// crossProcRejectPath builds the error verdict for an env-sourced path that
// failed validation. Safe to factor into a helper — unlike the path VALUE
// itself (see the inline-Clean note at each call site below), an error
// string is JSON-encoded to stdout, never passed to any gosec-tracked sink,
// so routing it through a function boundary carries no taint consequence.
func crossProcRejectPath(role, owner, raw string) crossProcVerdict {
	return crossProcVerdict{
		Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(),
		Err: fmt.Sprintf("%s: %q", errCrossProcPathNotValidated, raw),
	}
}

// On the inline filepath.Clean+IsAbs check duplicated at every call site
// below, instead of one shared validator (loto-nwuy):
//
// os.Getenv is a gosec taint SOURCE; without validation, gosec's G703
// (path traversal via taint analysis) follows LOTO_CROSSPROC_DB/TARGET/
// TARGETS inter-procedurally into chmod.go's safeOpenRegular and doctor.go's
// quarantine os.RemoveAll/os.Rename sinks — production files this bead does
// not touch, flagged only because this test binary is the first
// os.Getenv -> AcquireLocks chain gosec can see in the package.
//
// filepath.Clean is gosec's own recognized sanitizer for this analyzer
// (PathTraversal's Sanitizers list, gosec analyzers/pathtraversal.go) — but
// ONLY when the Clean call and its use sit in the SAME function body.
// gosec's interprocedural check for "does a tainted argument flow to a
// callee's return" (taint.go's doTaintedArgsFlowToReturn ->
// valueReachableFromParams) is a pure data-flow reachability walk that does
// NOT special-case an intervening sanitizer call — confirmed empirically: a
// crossProcValidatePath(v string) (string, error) helper that internally
// called filepath.Clean and returned it did NOT clear the taint, while
// inlining the identical Clean+IsAbs check directly at each call site does.
// So the value used at Open(dbPath)/AcquireLocks(...,target,...) below must
// be the literal, local result of filepath.Clean(os.Getenv(...)) — never
// laundered through a second function's return.

// blockerOwners flattens a *MultiConflictError's blockers to deduped owner
// uuid strings, so a child can report "who blocked me" across the process
// boundary without shipping typed domain values over JSON.
func blockerOwners(blockers []domain.LockRecord) []string {
	seen := make(map[string]bool, len(blockers))
	var out []string
	for i := range blockers {
		o := string(blockers[i].OwnerUUID)
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	return out
}

// crossProcMustWait reaps c and decodes its verdict. A non-zero exit
// (*exec.ExitError) is expected and NOT fatal — conflict/error/watchdog
// outcomes exit non-zero by design (crossproc_main_test.go's exit-code
// table). Anything else (a verdict that never decoded — broken pipe,
// crashed before printing) is a genuine harness failure.
func crossProcMustWait(t *testing.T, c *child) crossProcVerdict {
	t.Helper()
	v, err := c.wait()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("crossproc: child wait: %v", err)
	}
	return v
}

func containsOnly(vals []string, want string) bool {
	if len(vals) == 0 {
		return false
	}
	for _, v := range vals {
		if v != want {
			return false
		}
	}
	return true
}

func assertOwnerWriteStripped(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("crossproc: stat %s: %v", path, err)
	}
	if fi.Mode().Perm()&0o200 != 0 {
		t.Errorf("%s: owner-write bit set (mode=%o), want stripped", path, fi.Mode().Perm())
	}
}

func assertOwnerWritePresent(t *testing.T, path string) {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("crossproc: stat %s: %v", path, err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Errorf("%s: owner-write bit stripped (mode=%o), want present", path, fi.Mode().Perm())
	}
}

// rotateTargets returns a copy of targets rotated by i positions — A1 hands
// each child a distinct rotation so sortedByCanonical's ABBA defence
// (locks_acquire.go) is exercised across real processes, not just goroutines.
func rotateTargets(targets []string, i int) []string {
	n := len(targets)
	rot := make([]string, n)
	for j := range n {
		rot[j] = targets[(j+i)%n]
	}
	return rot
}

// ---- A1: contended acquire ----

// crossProcRoleAcquire is A1's child role: open the shared db, wait at the
// barrier, then race AcquireLocks against every sibling over the SAME
// target set (in this child's own rotated order). Nothing here is ever
// stale — liveProbe (liveprobe_test.go) reports every local pid alive — so
// the only question A1 asks is "who wins the write", not "who's dead".
func crossProcRoleAcquire() crossProcVerdict {
	const role = crossProcRoleAcquireName
	owner := os.Getenv(envOwner)

	dbPath := filepath.Clean(os.Getenv(envDBPath))
	if !filepath.IsAbs(dbPath) {
		return crossProcRejectPath(role, owner, dbPath)
	}
	rawTargets := strings.Split(os.Getenv(envTargets), ",")
	targets := make([]string, len(rawTargets))
	for i, rt := range rawTargets {
		vt := filepath.Clean(rt)
		if !filepath.IsAbs(vt) {
			return crossProcRejectPath(role, owner, vt)
		}
		targets[i] = vt
	}

	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}

	s, err := Open(dbPath)
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("open: %v", err)}
	}
	defer s.Close()

	now := time.Now()
	recs := make([]domain.LockRecord, len(targets))
	for i, p := range targets {
		recs[i] = domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   domain.AgentUUID(owner),
			SessionUUID: domain.SessionUUID(owner),
			Intent:      "crossproc-a1",
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        tcHost,
			PID:         os.Getpid(),
		}
	}

	_, acqErr := s.AcquireLocks(context.Background(), recs, liveProbe)
	if acqErr == nil {
		return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: owner, PID: os.Getpid(), PPID: os.Getppid()}
	}
	var mce *MultiConflictError
	if errors.As(acqErr, &mce) {
		return crossProcVerdict{
			Role: role, Outcome: crossProcConflict, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(),
			Err: acqErr.Error(), Blockers: blockerOwners(mce.Blockers),
		}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: acqErr.Error()}
}

// TestCrossProc_ContendedAcquire is A1 — the cross-process twin of
// TestConcurrentOverlappingAcquire (concurrent_test.go). N=8 real processes
// race AcquireLocks over the same M=3 targets, each submitting them in a
// distinct rotated order. Exactly one wins; every loser's *MultiConflictError
// names the winner; the DB and filesystem end consistent; nothing outside
// the declared target set is touched.
func TestCrossProc_ContendedAcquire(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	// Materialize schema, then close — every child opens its own handle.
	seed, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: materialize schema: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("crossproc: close seed store: %v", err)
	}

	const M = 3
	targets := make([]string, M)
	for i := range targets {
		p := filepath.Join(dir, fmt.Sprintf("target-%d.go", i))
		if err := os.WriteFile(p, []byte("package x\n"), 0o644); err != nil {
			t.Fatalf("crossproc: write target %d: %v", i, err)
		}
		targets[i] = p
	}
	// Never a lock target — proves the strip is scoped to the declared
	// write-set, not "every file in the temp dir".
	decoy := filepath.Join(dir, "decoy.go")
	if err := os.WriteFile(decoy, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("crossproc: write decoy: %v", err)
	}

	const N = 8
	b := newCrossProcBarrier(t)
	children := make([]*child, N)
	for i := range N {
		env := map[string]string{
			envDBPath:  dbPath,
			envOwner:   makeUUID(i),
			envTargets: strings.Join(rotateTargets(targets, i), ","),
		}
		children[i] = spawnChild(t, crossProcRoleAcquireName, env, b.startFile(), b.readyFile())
	}

	b.awaitReady(t, N)
	b.release()

	verdicts := make([]crossProcVerdict, 0, N)
	for _, c := range children {
		verdicts = append(verdicts, crossProcMustWait(t, c))
	}

	var winner *crossProcVerdict
	var losers []crossProcVerdict
	for i := range verdicts {
		if verdicts[i].Outcome == crossProcWon {
			if winner != nil {
				t.Fatalf("two winners: %s and %s", winner.Owner, verdicts[i].Owner)
			}
			winner = &verdicts[i]
			continue
		}
		losers = append(losers, verdicts[i])
	}
	if winner == nil {
		t.Fatalf("no winner among %d children: %+v", N, verdicts)
	}
	if len(losers) != N-1 {
		t.Fatalf("losers=%d, want %d: %+v", len(losers), N-1, verdicts)
	}
	for _, l := range losers {
		if l.Outcome != crossProcConflict {
			t.Errorf("loser %s outcome=%q, want conflict (err=%q)", l.Owner, l.Outcome, l.Err)
			continue
		}
		if !containsOnly(l.Blockers, winner.Owner) {
			t.Errorf("loser %s blockers=%v, want naming only winner %s", l.Owner, l.Blockers, winner.Owner)
		}
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	locks, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("crossproc: ListLocks: %v", err)
	}
	if len(locks) != M {
		t.Fatalf("locks=%d, want %d: %+v", len(locks), M, locks)
	}
	for _, l := range locks {
		if string(l.OwnerUUID) != winner.Owner {
			t.Errorf("lock %s owner=%q, want winner %q", l.Target.Canonical, l.OwnerUUID, winner.Owner)
		}
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("crossproc: ListEvents: %v", err)
	}
	acqCount := make(map[string]int, M)
	for _, e := range events {
		if e.Kind == EventLockAcquired {
			acqCount[e.Target.Canonical]++
		}
	}
	for _, tp := range targets {
		if acqCount[tp] != 1 {
			t.Errorf("target %s: %d lock_acquired events, want exactly 1", tp, acqCount[tp])
		}
	}

	for _, tp := range targets {
		assertOwnerWriteStripped(t, tp)
	}
	assertOwnerWritePresent(t, decoy)
}

// ---- A2: stale-reclaim TOCTOU ----

// crossProcRoleHold is A2's H: acquire one target stamping the REAL pid,
// signal the parent over fd 3 that the lock is genuinely held, then park.
// It never exits on its own — the parent SIGKILLs its whole process group
// once ready is observed, so "the holder is dead" is a fact by construction,
// not a race (plan §3 A2).
func crossProcRoleHold() crossProcVerdict {
	const role = crossProcRoleHoldName
	owner := os.Getenv(envOwner)

	dbPath := filepath.Clean(os.Getenv(envDBPath))
	if !filepath.IsAbs(dbPath) {
		return crossProcRejectPath(role, owner, dbPath)
	}
	target := filepath.Clean(os.Getenv(envTarget))
	if !filepath.IsAbs(target) {
		return crossProcRejectPath(role, owner, target)
	}

	s, err := Open(dbPath)
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("open: %v", err)}
	}

	now := time.Now()
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: target},
		OwnerUUID:   domain.AgentUUID(owner),
		SessionUUID: domain.SessionUUID(owner),
		Intent:      "crossproc-a2-hold",
		CreatedAt:   now,
		// Comfortably in the future: staleness in the A2 race must be
		// attributable ONLY to the dead-pid probe (this test's actual
		// subject), never to a TTL race.
		ExpiresAt: now.Add(time.Hour),
		Host:      crossProcA2Host,
		PID:       os.Getpid(), // the REAL pid — the point of this test
	}
	probe := crossProcRealPidProbe(crossProcA2Host)
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, probe); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("acquire: %v", err)}
	}

	readyW := os.NewFile(3, "crossproc-hold-ready")
	if _, err := readyW.Write([]byte{1}); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("signal ready: %v", err)}
	}
	_ = readyW.Close()

	// Park — no sleep, no CPU spin: block forever with nothing to receive.
	// The child self-watchdog (crossproc_main_test.go) is the backstop if
	// the parent ever fails to kill this process.
	select {}
}

// crossProcRoleReclaimAcquire is A2's R: wait at the race barrier, then
// race AcquireLocks for H's (now dead) target against every sibling using a
// probe that consults REAL pid liveness directly.
//
// After computing its verdict it does NOT exit immediately — it signals and
// parks at a SECOND barrier (fd 5/6) until every sibling has also computed
// its verdict. Without this, a fast-exiting winner would read DEAD to a
// sibling whose own transaction runs microseconds later: real-pid liveness
// can't distinguish "exited because it won" from "exited because it
// crashed", and SQLite's immediate-tx means siblings run strictly after the
// winner commits — so a sibling could otherwise reclaim the winner's own
// fresh row. This is exactly the trap the plan (§2.1) says the testscript
// LOTO_PID crutch exists to paper over; the fix here is to make the fact
// (still alive) true instead of faking it.
func crossProcRoleReclaimAcquire() crossProcVerdict {
	const role = crossProcRoleReclaimAcquireName
	owner := os.Getenv(envOwner)

	dbPath := filepath.Clean(os.Getenv(envDBPath))
	if !filepath.IsAbs(dbPath) {
		return crossProcRejectPath(role, owner, dbPath)
	}
	target := filepath.Clean(os.Getenv(envTarget))
	if !filepath.IsAbs(target) {
		return crossProcRejectPath(role, owner, target)
	}

	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}

	v := crossProcDoReclaimAcquire(role, dbPath, target, owner)

	if err := crossProcAwaitBarrierAt(5, 6); err != nil {
		v.Outcome = crossProcError
		v.Err = "exit-gate: " + err.Error()
	}
	return v
}

func crossProcDoReclaimAcquire(role, dbPath, target, owner string) crossProcVerdict {
	s, err := Open(dbPath)
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("open: %v", err)}
	}
	defer s.Close()

	now := time.Now()
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: target},
		OwnerUUID:   domain.AgentUUID(owner),
		SessionUUID: domain.SessionUUID(owner),
		Intent:      "crossproc-a2-reclaim",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        crossProcA2Host,
		PID:         os.Getpid(),
	}
	probe := crossProcRealPidProbe(crossProcA2Host)
	_, acqErr := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, probe)
	if acqErr == nil {
		return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: owner, PID: os.Getpid(), PPID: os.Getppid()}
	}
	var mce *MultiConflictError
	if errors.As(acqErr, &mce) {
		return crossProcVerdict{
			Role: role, Outcome: crossProcConflict, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(),
			Err: acqErr.Error(), Blockers: blockerOwners(mce.Blockers),
		}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: acqErr.Error()}
}

// TestCrossProc_StaleReclaimSingleWinner is A2 — loto-7wp.6's shape (b),
// which never landed (plan §3, §7): a real holder H acquires, is SIGKILLed
// and reaped BEFORE any reclaimer starts, then N=4 processes race the stale
// reclaim with a probe that checks real pid liveness directly. Exactly one
// wins; exactly one lock_reclaimed_stale event fires (two-to-four would be
// the TOCTOU signature); the target ends stripped under the new winner;
// H's row is gone.
func TestCrossProc_StaleReclaimSingleWinner(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	seed, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: materialize schema: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("crossproc: close seed store: %v", err)
	}

	target := filepath.Join(dir, "reclaim.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("crossproc: write target: %v", err)
	}

	hReadyR, hReadyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: H ready pipe: %v", err)
	}
	t.Cleanup(func() { _ = hReadyR.Close() })

	hChild := spawnChild(t, crossProcRoleHoldName, map[string]string{
		envDBPath: dbPath,
		envTarget: target,
		envOwner:  "h-holder",
	}, hReadyW)
	_ = hReadyW.Close() // parent doesn't write; only H's dup'd copy does

	buf := make([]byte, 1)
	if _, err := io.ReadFull(hReadyR, buf); err != nil {
		t.Fatalf("crossproc: await H ready: %v", err)
	}
	holderPID := hChild.cmd.Process.Pid

	// SIGKILL H's process group and reap BEFORE spawning anyone else — "the
	// holder is dead" is a fact from this point on, not a race. Reuses
	// spawnChild's documented deliberate-SIGKILL path (crossproc_main_test.go):
	// calling wait() here makes the child struct's own t.Cleanup a no-op.
	if pgid, err := syscall.Getpgid(holderPID); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = hChild.cmd.Wait()

	const N = 4
	raceGate := newCrossProcBarrier(t)
	exitGate := newCrossProcBarrier(t)
	children := make([]*child, N)
	for i := range N {
		env := map[string]string{
			envDBPath: dbPath,
			envTarget: target,
			envOwner:  fmt.Sprintf("r%d", i),
		}
		children[i] = spawnChild(t, crossProcRoleReclaimAcquireName, env,
			raceGate.startFile(), raceGate.readyFile(),
			exitGate.startFile(), exitGate.readyFile())
	}

	raceGate.awaitReady(t, N)
	raceGate.release()

	// Every reclaimer parks after computing its verdict until ALL N have —
	// see crossProcRoleReclaimAcquire for why this is load-bearing, not
	// cosmetic.
	exitGate.awaitReady(t, N)
	exitGate.release()

	verdicts := make([]crossProcVerdict, 0, N)
	var winner *crossProcVerdict
	for _, c := range children {
		v := crossProcMustWait(t, c)
		verdicts = append(verdicts, v)
		if v.Outcome == crossProcWon {
			if winner != nil {
				t.Fatalf("two winners: %s and %s", winner.Owner, v.Owner)
			}
			w := v
			winner = &w
		}
	}
	if winner == nil {
		t.Fatalf("no winner among %d reclaimers: %+v", N, verdicts)
	}
	for _, v := range verdicts {
		if v.Owner == winner.Owner {
			continue
		}
		if v.Outcome != crossProcConflict {
			t.Errorf("loser %s outcome=%q, want conflict (err=%q)", v.Owner, v.Outcome, v.Err)
		}
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()

	locks, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("crossproc: ListLocks: %v", err)
	}
	if len(locks) != 1 {
		t.Fatalf("locks=%d, want 1 (H's row must be gone): %+v", len(locks), locks)
	}
	if string(locks[0].OwnerUUID) != winner.Owner {
		t.Errorf("lock owner=%q, want winner %q", locks[0].OwnerUUID, winner.Owner)
	}

	events, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("crossproc: ListEvents: %v", err)
	}
	reclaimCount := 0
	for _, e := range events {
		if e.Kind == EventLockReclaimedStale && e.Target.Canonical == target {
			reclaimCount++
		}
	}
	if reclaimCount != 1 {
		t.Errorf("lock_reclaimed_stale events=%d, want exactly 1 (2-4 is the TOCTOU signature)", reclaimCount)
	}

	assertOwnerWriteStripped(t, target)

	t.Run("control/fault=no-flock", testCrossProcA2ControlNoFlock)
}

// ---- A2-control: fault=no-flock (see file header for why this shape, not
// the plan's literal bare rerun) ----

// crossProcRoleReclaimSharedFault is the A2-control's "S": with the
// op-flock neutered in this process, reclaim H's real-dead stale EXCLUSIVE
// row while acquiring the SAME target in SHARED mode — stripAll skips it
// (shared never strips), so restoreReclaimedSkippingRestripped's
// post-commit restore fires. See the file header for the full design and
// the flock-hold-across-restore proof this depends on.
//
// fchmodFn is hooked so the instant S is about to add owner-write back to
// the target, S signals the parent over fd 3 and blocks reading fd 4 — a
// deterministic stand-in for the flock's own contended-wakeup signal,
// unavailable here precisely because the fault has neutered it.
func crossProcRoleReclaimSharedFault() crossProcVerdict {
	const role = crossProcRoleReclaimSharedFaultName
	maybeInjectNoFlockFault()

	owner := os.Getenv(envOwner)
	dbPath := filepath.Clean(os.Getenv(envDBPath))
	if !filepath.IsAbs(dbPath) {
		return crossProcRejectPath(role, owner, dbPath)
	}
	target := filepath.Clean(os.Getenv(envTarget))
	if !filepath.IsAbs(target) {
		return crossProcRejectPath(role, owner, target)
	}

	s, err := Open(dbPath)
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("open: %v", err)}
	}
	defer s.Close()

	aboutToRestoreW := os.NewFile(3, "crossproc-about-to-restore")
	proceedR := os.NewFile(4, "crossproc-proceed")

	origFchmod := fchmodFn
	defer func() { fchmodFn = origFchmod }()
	signaled := false
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if !signaled && f.Name() == target && mode&0o200 != 0 {
			signaled = true
			if _, werr := aboutToRestoreW.Write([]byte{1}); werr != nil {
				return fmt.Errorf("crossproc: signal about-to-restore: %w", werr)
			}
			_ = aboutToRestoreW.Close()
			buf := make([]byte, 1)
			if _, rerr := proceedR.Read(buf); rerr != nil && !errors.Is(rerr, io.EOF) {
				return fmt.Errorf("crossproc: await proceed: %w", rerr)
			}
		}
		return origFchmod(f, mode)
	}

	now := time.Now()
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: target},
		OwnerUUID:   domain.AgentUUID(owner),
		SessionUUID: domain.SessionUUID(owner),
		Intent:      "crossproc-a2-control-shared",
		CreatedAt:   now,
		// Deliberately already elapsed — mirrors
		// TestAcquireLocks_HoldsFlockAcrossRestore's
		// alice.ExpiresAt = time.Now().Add(-time.Hour): makes S's OWN
		// freshly committed row immediately TTL-stale too, so E can
		// reclaim past it without needing S to look dead.
		ExpiresAt: now.Add(-time.Hour),
		Host:      crossProcA2Host,
		PID:       os.Getpid(),
		Mode:      domain.ModeShared,
	}
	probe := crossProcRealPidProbe(crossProcA2Host)
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, probe); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("acquire: %v", err)}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: owner, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcRoleReclaimExclusiveFault is the A2-control's "E": with the
// op-flock neutered in this process, acquire the same target EXCLUSIVELY.
// The parent spawns E only after S has signalled "about to restore", so
// E's whole acquire (validate, tx, strip, insert, commit) runs — and,
// absent the flock, is allowed to run — strictly between S's commit and
// S's restore.
func crossProcRoleReclaimExclusiveFault() crossProcVerdict {
	const role = crossProcRoleReclaimExclusiveFaultName
	maybeInjectNoFlockFault()

	owner := os.Getenv(envOwner)
	dbPath := filepath.Clean(os.Getenv(envDBPath))
	if !filepath.IsAbs(dbPath) {
		return crossProcRejectPath(role, owner, dbPath)
	}
	target := filepath.Clean(os.Getenv(envTarget))
	if !filepath.IsAbs(target) {
		return crossProcRejectPath(role, owner, target)
	}

	s, err := Open(dbPath)
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("open: %v", err)}
	}
	defer s.Close()

	now := time.Now()
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: target},
		OwnerUUID:   domain.AgentUUID(owner),
		SessionUUID: domain.SessionUUID(owner),
		Intent:      "crossproc-a2-control-exclusive",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        crossProcA2Host,
		PID:         os.Getpid(),
	}
	probe := crossProcRealPidProbe(crossProcA2Host)
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, probe); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: owner, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("acquire: %v", err)}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: owner, PID: os.Getpid(), PPID: os.Getppid()}
}

// testCrossProcA2ControlNoFlock is the A2-control body — see the file
// header for the full design and the flock-hold-across-restore proof. H
// (the existing "hold" role) supplies a genuinely dead real holder, exactly
// as in the parent test; S and E then run the two-actor choreography that
// reaches the flock's actual load-bearing job. If production's flock
// discipline somehow still applied here, the target would end stripped and
// this control has lost its teeth.
func testCrossProcA2ControlNoFlock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	seed, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: materialize schema: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("crossproc: close seed store: %v", err)
	}

	target := filepath.Join(dir, "control.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o644); err != nil {
		t.Fatalf("crossproc: write target: %v", err)
	}

	// H: real holder, killed and reaped before S ever spawns — same fact
	// A2's main scenario establishes, reusing the "hold" role as-is.
	hReadyR, hReadyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: H ready pipe: %v", err)
	}
	t.Cleanup(func() { _ = hReadyR.Close() })
	hChild := spawnChild(t, crossProcRoleHoldName, map[string]string{
		envDBPath: dbPath,
		envTarget: target,
		envOwner:  "control-h",
	}, hReadyW)
	_ = hReadyW.Close()
	hBuf := make([]byte, 1)
	if _, err := io.ReadFull(hReadyR, hBuf); err != nil {
		t.Fatalf("crossproc: await H ready: %v", err)
	}
	if pgid, err := syscall.Getpgid(hChild.cmd.Process.Pid); err == nil {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
	_ = hChild.cmd.Wait()

	aboutToRestoreR, aboutToRestoreW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: about-to-restore pipe: %v", err)
	}
	defer aboutToRestoreR.Close()
	proceedR, proceedW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: proceed pipe: %v", err)
	}
	defer proceedW.Close()

	sChild := spawnChild(t, crossProcRoleReclaimSharedFaultName, map[string]string{
		envDBPath: dbPath,
		envTarget: target,
		envOwner:  "control-s",
		envFault:  crossProcFaultNoFlock,
	}, aboutToRestoreW, proceedR)

	sBuf := make([]byte, 1)
	if _, err := io.ReadFull(aboutToRestoreR, sBuf); err != nil {
		t.Fatalf("crossproc: await S about-to-restore: %v", err)
	}

	eChild := spawnChild(t, crossProcRoleReclaimExclusiveFaultName, map[string]string{
		envDBPath: dbPath,
		envTarget: target,
		envOwner:  "control-e",
		envFault:  crossProcFaultNoFlock,
	})

	eV := crossProcMustWait(t, eChild)
	if eV.Outcome != crossProcWon {
		t.Fatalf("E outcome=%q, want won (err=%q) — control setup is broken, not demonstrating the fault", eV.Outcome, eV.Err)
	}

	_ = proceedW.Close() // let S's blocked restore proceed now that E has landed.

	sV := crossProcMustWait(t, sChild)
	if sV.Outcome != crossProcWon {
		t.Fatalf("S outcome=%q, want won (err=%q)", sV.Outcome, sV.Err)
	}

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("crossproc: reopen: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	locks, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("crossproc: ListLocks: %v", err)
	}
	if len(locks) != 1 || string(locks[0].OwnerUUID) != "control-e" {
		t.Fatalf("locks=%+v, want exactly E's (control-e) exclusive row", locks)
	}

	// The control: with the flock neutered, S's lagging restore must land
	// AFTER E's exclusive acquire — a reclaimed target ending
	// owner-writable while a live exclusive row exists.
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("crossproc: stat target: %v", err)
	}
	if fi.Mode().Perm()&0o200 == 0 {
		t.Fatalf("detection control lost its teeth: fault=no-flock injected but target ended read-only under E's live exclusive row — the violation was not observed")
	}
}
