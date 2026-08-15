//go:build unix

// Family C2 — PID-reuse liveness witness, plus the agent-GC pin set (loto-qhv
// build-order item 6, plan §3 Family C). Role functions live here and add
// their rows to crossProcRoles in crossproc_main_test.go; the spine itself is
// never duplicated.
//
// C2 invariant (plan §3): a lock whose recorded (pid, proc_start) witness no
// longer matches the process currently occupying that pid is DEAD, and a
// witness that cannot be read is UNKNOWN and never escalates to dead.
//
// Every case here runs against a REAL spawned process with a real pid and a
// real proc-start value read by the production reader (identity.ProcStart) —
// no test twin, no stubbed procStartFn, no stubbed procArgv. The oracle under
// test is Peer.SessionVerdict (liveness.go).
//
// One limit this file deliberately does NOT encode (plan §3): on Linux
// proc_start is in CONFIG_HZ clock ticks — commonly 10 ms granularity — so
// two processes started inside the same tick share a value. "Two distinct
// children have distinct proc_start" is therefore flaky by construction and
// is never asserted. Every comparison here is equality against a value read
// on this host, which is the only contract Peer.ProcStart documents.
package identity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Role names. Referenced by crossProcRoles in crossproc_main_test.go.
const (
	crossProcRoleParkName     = "park"
	crossProcRoleGCHolderName = "gc-holder"
)

// crossProcArgvAgentID is stamped into a park child's REAL argv (via
// spawnChildArgs) so the oracle's argv branch reads it back out of the proc
// table. crossProcArgvOtherID is the same shape belonging to nobody — the
// recorded-identity half of the argv-mismatch case.
const (
	crossProcArgvAgentID = "loto-qhv-c2@team"
	crossProcArgvOtherID = "loto-qhv-c2-other@team"
)

// crossProcRolePark is C2's subject process: it exists, holds a real pid with
// a real OS start-time, and does nothing else. It parks at the barrier so the
// parent can read its pid and take verdicts against it while it is provably
// alive, and exits once released. Deliberately identity-agnostic — it never
// calls Ensure, so a liveness failure can never be an identity failure in
// disguise.
func crossProcRolePark() crossProcVerdict {
	const role = crossProcRoleParkName
	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcRoleGCHolder is the GC pin-set choreography's live holder: resolve
// an identity, age its own agent record past agentsGCMaxAge so the reaper is
// entitled to take it, then park while the parent runs a GC pass in a
// DIFFERENT process. On release it reports whether it can still resolve its
// own record — the observation that matters, taken from the holder's side of
// the process boundary rather than the reaper's.
//
// Outcome vocabulary here: won = the record survived, conflict = the reaper
// took it out from under a live holder (the gh#125 / loto-ffg red).
//
// agentsGCOnce is a per-process sync.Once, which is a second reason the GC
// pass and the holder must be separate processes: two GCAgents calls in one
// test binary would silently collapse into one.
func crossProcRoleGCHolder() crossProcVerdict {
	const role = crossProcRoleGCHolderName
	a, err := Ensure(context.Background())
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	old := time.Now().Add(-2 * agentsGCMaxAge)
	if err := os.Chtimes(filepath.Join(registryDir(), a.UUID+".json"), old, old); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	if _, err := LookupByUUID(a.UUID); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcConflict, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcRequireProcTable skips the calling test when this host will not
// hand back a process's start-time or command line (a sandbox with no /proc,
// a denied sysctl, a `ps` that truncates its output). Both are real-witness
// preconditions, not the contract under test: SessionVerdict's
// unreadable-witness branches stay covered by the in-process liveness_test.go
// (plan §6).
func crossProcRequireProcTable(t *testing.T) {
	t.Helper()
	if _, ok := ProcStart(os.Getpid()); !ok {
		t.Skip("crossproc: no readable proc start-time on this host")
	}
	if procArgv(t.Context(), os.Getpid()) == "" {
		t.Skip("crossproc: no readable proc command line on this host")
	}
}

// crossProcParkedChild spawns one park-role child with the given argv tail and
// blocks until it has reached the barrier — i.e. until it is provably a live
// process. Returns the child and its pid. The caller releases the barrier when
// it is done taking verdicts against it.
func crossProcParkedChild(t *testing.T, args []string) (*child, *crossProcBarrier, int) {
	t.Helper()
	b := newCrossProcBarrier(t)
	c := spawnChildArgs(t, crossProcRoleParkName, "", nil, args, b.startFile(), b.readyFile())
	b.awaitReady(t, 1)
	return c, b, c.cmd.Process.Pid
}

// TestCrossProc_RecycledPIDIsDead is C2's witness table: four verdicts taken
// against real processes on this host, with the production ProcStart reader
// and the production `ps` argv reader both live.
//
// On "simulated recycle" being honest (plan §3, decision log): the OS will not
// recycle a pid on demand — /proc/sys/kernel/ns_last_pid needs privileges and
// is itself racy, and spinning until pid_max wraps costs minutes of CPU. So
// the recycle case is expressed as a pid that is genuinely live and genuinely
// occupied by a process we spawned, carrying a stamped witness that belongs to
// a different process-instance (here: procStart(pid)−1, a value this pid
// provably does not have). The OS did not recycle anything; the discriminating
// input the contract must handle is nonetheless exactly reproduced.
func TestCrossProc_RecycledPIDIsDead(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}
	crossProcRequireProcTable(t)

	ctx := t.Context()
	// A plain file stands in for the messaging socket: SessionVerdict only
	// os.Stats p.Socket, it never dials, so no listener is needed — and a real
	// unix listener under a long /var/folders temp dir would risk darwin's
	// ~104-byte sun_path limit for no added coverage.
	sock := existingSocket(t)

	live, liveBarrier, livePID := crossProcParkedChild(t, nil)
	defer func() {
		liveBarrier.release()
		crossProcMustWait(t, live)
	}()
	liveStart, ok := ProcStart(livePID)
	if !ok {
		t.Fatalf("ProcStart(%d) failed for a live child", livePID)
	}

	doomed, doomedBarrier, _ := crossProcParkedChild(t, nil)
	doomedPID := doomed.cmd.Process.Pid
	doomedStart, ok := ProcStart(doomedPID)
	if !ok {
		t.Fatalf("ProcStart(%d) failed for a live child", doomedPID)
	}
	crossProcKillAndReap(t, doomed)
	doomedBarrier.release()

	tests := []struct {
		name         string
		peer         Peer
		wantLiveness SessionLiveness
		wantReason   string
	}{
		{
			// The witness names this exact process-instance: proven identical
			// process, so the oracle short-circuits the ps exec entirely.
			name:         "witness match",
			peer:         Peer{Socket: sock, PID: livePID, ProcStart: liveStart},
			wantLiveness: SessionLive,
			wantReason:   reasonProcStartMatch,
		},
		{
			// Simulated recycle — see the function comment. Live pid, witness
			// from another instance.
			name:         "simulated recycle",
			peer:         Peer{Socket: sock, PID: livePID, ProcStart: liveStart - 1},
			wantLiveness: SessionDead,
			wantReason:   reasonProcStartMismatch,
		},
		{
			// Killed and reaped before this verdict is taken, so "the holder is
			// dead" is a fact rather than a race. The pid check precedes the
			// witness check, so the witness recorded pre-death never gets read.
			name:         "dead pid",
			peer:         Peer{Socket: sock, PID: doomedPID, ProcStart: doomedStart},
			wantLiveness: SessionDead,
			wantReason:   reasonPIDDead,
		},
		{
			// The false-positive control, and the class's red: a legacy row
			// whose proc_start column was NULL maps to 0 = UNKNOWN. Unknown is
			// not evidence, so the oracle degrades to the argv check — which a
			// top-level session (no --agent-id, no --parent-session-id, exactly
			// this park child) cannot fail. A holder whose recorded identity
			// belongs to some other instance therefore reads ALIVE and its lock
			// is never reclaimed. This is the deliberate NULL→0→UNKNOWN trade:
			// it degrades toward availability, not correctness.
			name:         "unknown witness never escalates to dead",
			peer:         Peer{Socket: sock, PID: livePID, ProcStart: 0},
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.peer.SessionVerdict(ctx)
			if got.Liveness != tc.wantLiveness {
				t.Errorf("liveness = %v, want %v (reason=%q)", got.Liveness, tc.wantLiveness, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}

	// The red stated as the pair it actually is (plan §3, "How C is shown
	// RED"): the witness is the whole discriminator. Same live pid, same
	// socket, same everything else — present-and-foreign witness flips the
	// verdict to DEAD, absent witness leaves it ALIVE. If these ever agree,
	// the witness has stopped carrying signal and the table above would pass
	// while proving nothing.
	withWitness := Peer{Socket: sock, PID: livePID, ProcStart: liveStart - 1}.SessionVerdict(ctx)
	withoutWitness := Peer{Socket: sock, PID: livePID}.SessionVerdict(ctx)
	if withWitness.Liveness == withoutWitness.Liveness {
		t.Errorf("witness carries no discriminating power: with-witness=%v (%s), without-witness=%v (%s)",
			withWitness.Liveness, withWitness.Reason, withoutWitness.Liveness, withoutWitness.Reason)
	}
}

// TestCrossProc_ArgvIdentityDiscriminatesOccupant is the argv half of the C2
// table: with the start-time witness UNKNOWN, the oracle falls through to the
// recorded identity flags and compares them against the pid's REAL command
// line. Both rows run against the same live child, whose argv genuinely
// carries --agent-id — no stubbed procArgv.
func TestCrossProc_ArgvIdentityDiscriminatesOccupant(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}
	crossProcRequireProcTable(t)

	ctx := t.Context()
	sock := existingSocket(t)

	c, b, pid := crossProcParkedChild(t, []string{"--agent-id", crossProcArgvAgentID})
	defer func() {
		b.release()
		crossProcMustWait(t, c)
	}()

	// Precondition, not an assertion about the oracle: some `ps` builds clip a
	// long command line, and the test binary's own path is long. If the flag
	// did not survive the round trip there is no argv signal to discriminate
	// on, and pretending otherwise would fake the case rather than test it.
	if got := argvFlag(procArgv(ctx, pid), "agent-id"); got != crossProcArgvAgentID {
		t.Skipf("crossproc: proc table does not expose the child's --agent-id (read %q, want %q)", got, crossProcArgvAgentID)
	}

	tests := []struct {
		name         string
		recorded     string
		wantLiveness SessionLiveness
		wantReason   string
	}{
		{
			name:         "argv identity matches",
			recorded:     crossProcArgvAgentID,
			wantLiveness: SessionLive,
			wantReason:   reasonPSMatch,
		},
		{
			name:         "argv identity mismatch",
			recorded:     crossProcArgvOtherID,
			wantLiveness: SessionDead,
			wantReason:   reasonArgvMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// ProcStart deliberately 0: a readable witness would short-circuit
			// the argv branch this case exists to exercise.
			p := Peer{Socket: sock, PID: pid, AgentID: tc.recorded}
			got := p.SessionVerdict(ctx)
			if got.Liveness != tc.wantLiveness {
				t.Errorf("liveness = %v, want %v (reason=%q)", got.Liveness, tc.wantLiveness, got.Reason)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

// crossProcGCHome starts a gc-holder child under a fresh HOME that the parent
// also adopts (registryDir/sessionDir resolve through $HOME), waits for it to
// park, and returns the barrier, the child, and the uuid it resolved. On
// return the child is alive, its agent record is aged past agentsGCMaxAge, and
// its session cache has been freed — so the caller's pin set is the only thing
// left between that record and the reaper.
func crossProcGCHome(t *testing.T, sid string) (*child, *crossProcBarrier, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv(crossProcHomeEnv, home)

	b := newCrossProcBarrier(t)
	c := spawnChild(t, crossProcRoleGCHolderName, "",
		map[string]string{crossProcHomeEnv: home, crossProcSessionIDEnv: sid},
		b.startFile(), b.readyFile())
	b.awaitReady(t, 1)

	uuid := crossProcSoleAgentUUID(t, home)
	crossProcFreeSessionPin(t)
	return c, b, uuid
}

// crossProcSoleAgentUUID returns the uuid of the one agent record under home,
// failing if there is not exactly one. The holder child reports its uuid only
// in its exit verdict, which is not readable while it is still parked — the
// registry itself is the cheaper handle.
func crossProcSoleAgentUUID(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".loto", "agents")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("crossproc: read agents dir: %v", err)
	}
	uuids := make([]string, 0, 1)
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".json"); ok {
			uuids = append(uuids, name)
		}
	}
	if len(uuids) != 1 {
		t.Fatalf("agents dir holds %d records, want exactly 1: %v", len(uuids), uuids)
	}
	return uuids[0]
}

// crossProcFreeSessionPin ages every session cache file under the current HOME
// past agentsGCMaxAge and runs the production GCSessions pass over them.
//
// This is the documented doctor ordering — sessions first, then agents
// (registry_test.go's TestGCSessionsThenGCAgentsFreesPins) — and it is the
// state the pin set exists for: with no session cache referencing the uuid,
// the lock-owner pin set passed to GCAgents is the ONLY thing standing between
// a live holder's record and the reaper. Without this step gcStaleAgents would
// keep the record for the session pin alone and the pin-set assertions would
// pass vacuously.
func crossProcFreeSessionPin(t *testing.T) {
	t.Helper()
	dir := sessionDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("crossproc: read session dir: %v", err)
	}
	old := time.Now().Add(-2 * agentsGCMaxAge)
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("crossproc: age session cache %s: %v", path, err)
		}
	}
	if _, _, err := GCSessions(time.Now(), "", nil); err != nil {
		t.Fatalf("crossproc: GCSessions: %v", err)
	}
	if left := sessionReferencedUUIDs(); len(left) != 0 {
		t.Fatalf("session cache still pins %d uuid(s) after GCSessions: %v", len(left), left)
	}
}

// TestCrossProc_GCPinnedAgentSurvives is the GC pin set's green half: a GC
// pass run in a different process from the holder must not reap an agent
// record named in the pin set, however stale that record's mtime is. The pin
// set is GCAgents' map[string]struct{} parameter — the seam the CLI runtime
// fills with owner_uuids from live lock rows, since identity cannot import
// store.
func TestCrossProc_GCPinnedAgentSurvives(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	c, b, uuid := crossProcGCHome(t, "sid-gc-pinned")

	// agentsGCOnce is process-wide; clear it so this GCAgents call actually
	// runs rather than no-op'ing behind some earlier test in the same binary.
	ResetGCOnceForTests()
	if err := GCAgents(time.Now(), map[string]struct{}{uuid: {}}); err != nil {
		t.Fatalf("GCAgents: %v", err)
	}
	if _, err := LookupByUUID(uuid); err != nil {
		t.Errorf("pinned agent %s must survive GC, LookupByUUID = %v", uuid, err)
	}

	b.release()
	v := crossProcMustWait(t, c)
	if v.Outcome != crossProcWon {
		t.Errorf("holder outcome = %q, want won — a live, pinned holder must still resolve its own record (err=%q)", v.Outcome, v.Err)
	}
	if v.Owner != uuid {
		t.Errorf("holder resolved uuid %q, registry held %q", v.Owner, uuid)
	}
}

// TestCrossProc_GCUnpinnedAgentIsReaped is the same choreography with an empty
// pin set, and it is the gh#125 / loto-ffg red across a real process boundary:
// the reaper takes a stale-by-mtime record while its holder is still running,
// and the holder — a live process — can no longer resolve its own identity.
// gcStaleAgents is called directly rather than through GCAgents because
// agentsGCOnce would swallow a second call in this binary.
func TestCrossProc_GCUnpinnedAgentIsReaped(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	c, b, uuid := crossProcGCHome(t, "sid-gc-unpinned")

	if err := gcStaleAgents(time.Now(), nil); err != nil {
		t.Fatalf("gcStaleAgents: %v", err)
	}
	if _, err := LookupByUUID(uuid); err == nil {
		t.Errorf("unpinned stale agent %s survived GC — the pin set is not what kept it alive in the pinned case, so that assertion would be vacuous", uuid)
	}

	b.release()
	v := crossProcMustWait(t, c)
	if v.Outcome != crossProcConflict {
		t.Errorf("holder outcome = %q, want conflict — a live holder whose record was reaped must fail to resolve it (err=%q)", v.Outcome, v.Err)
	}
	if v.Owner != uuid {
		t.Errorf("holder resolved uuid %q, registry held %q", v.Owner, uuid)
	}
}
