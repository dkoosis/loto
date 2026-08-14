//go:build unix

// Family C1 — identity stability across real cross-process churn (loto-qhv
// build-order item 5, plan §3 Family C). Role functions live here, same
// split as internal/store/crossproc_acquire_test.go: crossproc_main_test.go
// carries only the spine + its own smoke role; this file both defines the
// C1 role functions and adds their rows to crossProcRoles, and holds the
// tests that drive them.
//
// C1 invariant (plan §3): the agent uuid a process resolves depends only on
// (HOME, identity env binding) — never on PPID, cwd, argv0, or the number of
// concurrent resolvers — and concurrent first use of one session id
// converges on exactly one uuid backed by exactly one on-disk agent record.
package identity

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// crossProcSessionIDEnv and crossProcHomeEnv name the two env vars every C1
// case sets to control identity resolution (registry.go's homeDir/Ensure).
// Named once here so goconst doesn't flag their many call-site repeats.
const (
	crossProcSessionIDEnv = "CLAUDE_CODE_SESSION_ID"
	crossProcHomeEnv      = "HOME"
)

// Role names. Referenced by crossProcRoles in crossproc_main_test.go.
const (
	crossProcRoleEnsureName          = "ensure"
	crossProcRoleReexecName          = "reexec"
	crossProcRoleSpawnGrandchildName = "spawn-grandchild"
)

// crossProcRoleEnsure is C1's workhorse role: wait at the barrier, then
// resolve the current process's identity via Ensure and report its uuid.
// Used directly by C1c/C1d/C1e and, forwarded through
// crossProcRoleSpawnGrandchild, by C1a's reparented grandchild.
func crossProcRoleEnsure() crossProcVerdict {
	const role = crossProcRoleEnsureName
	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	a, err := Ensure(context.Background())
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcPrevUUIDEnv carries the pre-exec uuid across a syscall.Exec —
// crossProcRoleReexec's own in-memory state does not survive the exec, so
// the value has to ride in the replacement image's environment instead.
const crossProcPrevUUIDEnv = "LOTO_CROSSPROC_PREV_UUID"

// crossProcRoleReexec is C1b's role, in two stages distinguished by whether
// crossProcPrevUUIDEnv is set — both stages run under the SAME pid, since
// syscall.Exec replaces the process image without forking:
//
//  1. (unset) resolve Ensure, then syscall.Exec this same binary with an
//     identical curated env plus the resolved uuid stamped in
//     crossProcPrevUUIDEnv. On success this never returns.
//  2. (set) resolve Ensure again and compare against the stamped uuid,
//     emitting the one verdict line the parent actually reads.
//
// No barrier: this role runs one child at a time — the invariant it checks
// (identity survives a real re-exec) needs no cross-process choreography.
func crossProcRoleReexec() crossProcVerdict {
	const role = crossProcRoleReexecName
	if prev := os.Getenv(crossProcPrevUUIDEnv); prev != "" {
		a, err := Ensure(context.Background())
		if err != nil {
			return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
		}
		if a.UUID != prev {
			return crossProcVerdict{
				Role: role, Outcome: crossProcConflict, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid(),
				Err: fmt.Sprintf("post-exec uuid %s != pre-exec uuid %s", a.UUID, prev),
			}
		}
		return crossProcVerdict{Role: role, Outcome: crossProcWon, Owner: a.UUID, PID: os.Getpid(), PPID: os.Getppid()}
	}

	a, err := Ensure(context.Background())
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	exe, err := os.Executable()
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	// os.Environ() here is exactly the curated env crossProcChildEnv built for
	// this process (exec.Cmd.Env fully replaces, never inherits-plus) — no
	// second construction needed, and nothing of the grandparent test
	// process's own environment leaks in.
	env := append(os.Environ(), crossProcPrevUUIDEnv+"="+a.UUID)
	if err := syscall.Exec(exe, []string{exe}, env); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("exec: %v", err)}
	}
	return crossProcVerdict{} // unreachable on success
}

// crossProcRoleSpawnGrandchild is C1a's "G": spawn a grandchild ("GG",
// running the ensure role) and exit immediately, without waiting. GG is not
// killed by G's exit (unix does not signal children on parent exit) — it is
// reparented to the nearest subreaper (pid 1, or the platform's session
// init), giving TestCrossProc_IdentityStableAcrossPPIDChurn a REAL PPID
// change to observe rather than a simulated one.
//
// GG's stdout is not proxied through G's own stdout capture (a
// *bytes.Buffer copy loop that dies with G) — the parent instead hands G a
// dedicated report pipe (fd 5) that G assigns directly as GG's Stdout via
// exec.Cmd, so the OS dup2's it straight into GG's own fd table. The pipe
// then survives G's exit and reaches EOF only when GG itself closes it —
// i.e. when GG actually finishes, not when G does.
const crossProcGGReportFd = 5

func crossProcRoleSpawnGrandchild() crossProcVerdict {
	const role = crossProcRoleSpawnGrandchildName
	startF := os.NewFile(3, "crossproc-gg-start")
	readyF := os.NewFile(4, "crossproc-gg-ready")
	reportF := os.NewFile(crossProcGGReportFd, "crossproc-gg-report")

	exe, err := os.Executable()
	if err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}

	cmd := exec.Command(exe)
	cmd.Env = crossProcOverrideRole(os.Environ(), crossProcRoleEnsureName)
	cmd.ExtraFiles = []*os.File{startF, readyF} // GG's fd 3/4 — crossProcAwaitBarrier's hardcoded pair
	cmd.Stdout = reportF
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: fmt.Sprintf("start grandchild: %v", err)}
	}
	// GG now holds its own dup of fd 3/4/5 from the exec syscall; release G's
	// copies immediately so G's imminent exit doesn't read as "still holding
	// the report pipe open" — GG must be left as the pipe's sole writer, or
	// the parent's read on the other end would never see EOF.
	_ = startF.Close()
	_ = readyF.Close()
	_ = reportF.Close()

	// No Wait(): exiting now, without reaping GG, is the point. GG's own
	// per-process watchdog (armed by runCrossProcRole the instant it starts)
	// bounds it if anything goes wrong; nothing here needs to track its pid.
	return crossProcVerdict{Role: role, Outcome: crossProcWon, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcOverrideRole returns a copy of env with crossProcRoleEnv set to
// role, replacing any existing entry — used by crossProcRoleSpawnGrandchild
// to hand GG an identical HOME/session-id environment to G's own, with only
// the role changed.
func crossProcOverrideRole(env []string, role string) []string {
	out := make([]string, 0, len(env)+1)
	prefix := crossProcRoleEnv + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return append(out, prefix+role)
}

// runEnsureChild spawns one "ensure"-role child under its own single-child
// barrier (trivial N=1 — release happens immediately after the one ready
// byte arrives) and returns its verdict, failing the test if the child
// didn't report outcome=won. Used by every C1 case that just needs "resolve
// identity once, under this env/cwd, and tell me the uuid".
func runEnsureChild(t *testing.T, dir string, env map[string]string) crossProcVerdict {
	t.Helper()
	b := newCrossProcBarrier(t)
	c := spawnChild(t, crossProcRoleEnsureName, dir, env, b.startFile(), b.readyFile())
	b.awaitReady(t, 1)
	b.release()
	v := crossProcMustWait(t, c)
	if v.Outcome != crossProcWon {
		t.Fatalf("ensure child outcome = %q, want won (err=%q)", v.Outcome, v.Err)
	}
	return v
}

// dirSnapshot returns path → sha256(content) for every regular file
// directly inside dir, or an empty map if dir doesn't exist yet. Used by
// C1d to prove HOME1's registry is byte-identical before and after a
// resolution under a completely different HOME2.
func dirSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]string{}
		}
		t.Fatalf("crossproc: read dir %s: %v", dir, err)
	}
	out := make(map[string]string, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("crossproc: read %s: %v", filepath.Join(dir, e.Name()), err)
		}
		sum := sha256.Sum256(body)
		out[e.Name()] = hex.EncodeToString(sum[:])
	}
	return out
}

// TestCrossProc_IdentityStableAcrossPPIDChurn is C1a: a grandchild GG,
// reparented mid-life by G's exit (a real PPID change, not a simulated
// one — see crossProcRoleSpawnGrandchild), resolves the same uuid as a
// direct child given the identical HOME + CLAUDE_CODE_SESSION_ID.
func TestCrossProc_IdentityStableAcrossPPIDChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home := t.TempDir()
	env := map[string]string{crossProcHomeEnv: home, crossProcSessionIDEnv: "sid-ppid-churn"}

	direct := runEnsureChild(t, "", env)

	reportR, reportW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: gg report pipe: %v", err)
	}
	t.Cleanup(func() { _ = reportR.Close() })

	b := newCrossProcBarrier(t)
	g := spawnChild(t, crossProcRoleSpawnGrandchildName, "", env, b.startFile(), b.readyFile(), reportW)
	// The test's own copy of reportW must go, or GG closing its dup at exit
	// would not be the LAST writer — the read below would then block past
	// GG's own completion, waiting on a reference this process itself held.
	if err := reportW.Close(); err != nil {
		t.Fatalf("crossproc: close parent's report-pipe write end: %v", err)
	}

	gv := crossProcMustWait(t, g) // reaps G — by the time this returns, GG has already been reparented
	if gv.Outcome != crossProcWon {
		t.Fatalf("G outcome = %q, want won (err=%q)", gv.Outcome, gv.Err)
	}

	b.awaitReady(t, 1)
	b.release()

	// GG's own watchdog bounds it to crossProcWatchdog; this deadline is a
	// second, generous backstop on the parent's read, not a synchronization
	// mechanism — the happy path never approaches it.
	if err := reportR.SetReadDeadline(time.Now().Add(crossProcWatchdog + 5*time.Second)); err != nil {
		t.Fatalf("crossproc: set gg report deadline: %v", err)
	}
	var gg crossProcVerdict
	if err := json.NewDecoder(reportR).Decode(&gg); err != nil {
		t.Fatalf("crossproc: decode grandchild verdict: %v", err)
	}

	if gg.Outcome != crossProcWon {
		t.Fatalf("GG outcome = %q, want won (err=%q)", gg.Outcome, gg.Err)
	}
	if gg.PID == gv.PID {
		t.Errorf("GG pid = %d, same as G's — grandchild spawn didn't happen", gg.PID)
	}
	// The discrimination check the plan requires (§3 C1a): prove the PPID
	// genuinely changed, or this test would pass vacuously as "GG's parent
	// happens to equal G" without ever observing a reparent.
	if gg.PPID == gv.PID {
		t.Errorf("GG ppid = %d, still G's pid %d — not reparented", gg.PPID, gv.PID)
	}
	if gg.PPID == os.Getpid() {
		t.Errorf("GG ppid = %d, equals the test process — reparented to us, not orphan-adopted", gg.PPID)
	}
	if gg.Owner != direct.Owner {
		t.Errorf("GG resolved uuid %q, direct child resolved %q, want equal", gg.Owner, direct.Owner)
	}
}

// TestCrossProc_IdentityStableAcrossReexec is C1b: a process that resolves
// its identity, then syscall.Execs itself (same pid, replaced image, same
// curated env) and resolves again, gets the same uuid both times.
func TestCrossProc_IdentityStableAcrossReexec(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home := t.TempDir()
	env := map[string]string{crossProcHomeEnv: home, crossProcSessionIDEnv: "sid-reexec"}
	c := spawnChild(t, crossProcRoleReexecName, "", env)
	v := crossProcMustWait(t, c)
	if v.Outcome != crossProcWon {
		t.Fatalf("outcome = %q, want won (err=%q)", v.Outcome, v.Err)
	}
	if v.Owner == "" {
		t.Errorf("resolved uuid is empty")
	}
}

// TestCrossProc_IdentityStableAcrossCwdChurn is C1c: two children with the
// same HOME but different cwd — one of them a directory that is not under
// HOME — resolve the same uuid. homeDir()'s $HOME-first lookup exists
// precisely so identity can't fragment by cwd (gh#112/loto-3axo); this
// proves it holds across real processes, not just in-process calls.
func TestCrossProc_IdentityStableAcrossCwdChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home := t.TempDir()
	outside := t.TempDir() // a sibling of home, not a descendant of it
	env := map[string]string{crossProcHomeEnv: home, crossProcSessionIDEnv: "sid-cwd-churn"}

	inHome := runEnsureChild(t, home, env)
	outHome := runEnsureChild(t, outside, env)

	if inHome.Owner != outHome.Owner {
		t.Errorf("uuid diverges by cwd: cwd=HOME -> %q, cwd=outside-HOME -> %q", inHome.Owner, outHome.Owner)
	}
}

// TestCrossProc_IdentityDivergesAcrossHomeChurn is C1d: the same session id
// resolved under two different HOMEs must yield different uuids, and
// HOME1's registry + session cache must be byte-identical before and after
// the HOME2 resolution — proving the two are genuinely isolated, not just
// coincidentally different.
func TestCrossProc_IdentityDivergesAcrossHomeChurn(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home1 := t.TempDir()
	home2 := t.TempDir()
	sid := "sid-home-churn"

	v1 := runEnsureChild(t, "", map[string]string{crossProcHomeEnv: home1, crossProcSessionIDEnv: sid})
	agentsBefore := dirSnapshot(t, filepath.Join(home1, ".loto", "agents"))
	sessionsBefore := dirSnapshot(t, filepath.Join(home1, ".loto", "session"))

	v2 := runEnsureChild(t, "", map[string]string{crossProcHomeEnv: home2, crossProcSessionIDEnv: sid})

	if v1.Owner == v2.Owner {
		t.Errorf("uuid %q resolved under both HOME1 and HOME2 for the same session id, want divergence", v1.Owner)
	}

	agentsAfter := dirSnapshot(t, filepath.Join(home1, ".loto", "agents"))
	sessionsAfter := dirSnapshot(t, filepath.Join(home1, ".loto", "session"))
	if !maps.Equal(agentsBefore, agentsAfter) {
		t.Errorf("HOME1's agent registry changed after resolving under HOME2: before=%v after=%v", agentsBefore, agentsAfter)
	}
	if !maps.Equal(sessionsBefore, sessionsAfter) {
		t.Errorf("HOME1's session cache changed after resolving under HOME2: before=%v after=%v", sessionsBefore, sessionsAfter)
	}
}

// TestCrossProc_ConcurrentFirstUseConverges is C1e: N=8 processes, same
// fresh session id, same HOME, released from one barrier at once, all
// converge on the same uuid via claimSessionCache's O_CREATE|O_EXCL
// winner-take-all plus ensureForSession's loser retry/recovery path —
// machinery written for concurrent *processes* that, before this bead, had
// only ever been exercised by concurrent goroutines.
func TestCrossProc_ConcurrentFirstUseConverges(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home := t.TempDir()
	env := map[string]string{crossProcHomeEnv: home, crossProcSessionIDEnv: "sid-concurrent-first-use"}
	const n = 8

	b := newCrossProcBarrier(t)
	children := make([]*child, n)
	for i := range children {
		children[i] = spawnChild(t, crossProcRoleEnsureName, "", env, b.startFile(), b.readyFile())
	}
	b.awaitReady(t, n)
	b.release()

	uuids := make(map[string]int, 1)
	for _, c := range children {
		v := crossProcMustWait(t, c)
		if v.Outcome != crossProcWon {
			t.Fatalf("child outcome = %q, want won (err=%q)", v.Outcome, v.Err)
		}
		uuids[v.Owner]++
	}
	if len(uuids) != 1 {
		t.Errorf("children converged on %d distinct uuids, want 1: %v", len(uuids), uuids)
	}

	agents := dirSnapshot(t, filepath.Join(home, ".loto", "agents"))
	if len(agents) != 1 {
		t.Errorf("agents dir holds %d records, want exactly 1 (no orphaned loser candidate files): %v", len(agents), agents)
	}
	sessions := dirSnapshot(t, filepath.Join(home, ".loto", "session"))
	if len(sessions) != 1 {
		t.Errorf("session dir holds %d records, want exactly 1: %v", len(sessions), sessions)
	}
}

// TestCrossProc_ConcurrentFirstUseConverges_ControlDiverges is C1e's
// discrimination control (plan §3): the same N=8 barriered choreography,
// but with LOTO_AGENT_ID="" (explicit-ephemeral) instead of a shared
// session id, so each child mints its OWN throwaway identity independently.
// If this converged too, the main test's "all 8 match" assertion would be
// vacuous — it would prove nothing about concurrent-first-use convergence,
// only that Ensure always returns the same thing regardless of input.
func TestCrossProc_ConcurrentFirstUseConverges_ControlDiverges(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	home := t.TempDir()
	env := map[string]string{crossProcHomeEnv: home, "LOTO_AGENT_ID": ""}
	const n = 8

	b := newCrossProcBarrier(t)
	children := make([]*child, n)
	for i := range children {
		children[i] = spawnChild(t, crossProcRoleEnsureName, "", env, b.startFile(), b.readyFile())
	}
	b.awaitReady(t, n)
	b.release()

	uuids := make(map[string]int, n)
	for _, c := range children {
		v := crossProcMustWait(t, c)
		if v.Outcome != crossProcWon {
			t.Fatalf("child outcome = %q, want won (err=%q)", v.Outcome, v.Err)
		}
		uuids[v.Owner]++
	}
	if len(uuids) != n {
		t.Fatalf("control: children converged on %d distinct uuids, want %d — LOTO_AGENT_ID=\"\" must mint independently per child, or the main test's convergence assertion is vacuous: %v", len(uuids), n, uuids)
	}

	// bindingAgentIDEphemeral resolves via mintAgent, not newAgent — an
	// explicit-ephemeral identity is documented as a throwaway and is never
	// written to ~/.loto/agents/ (registry.go). So the discriminating
	// assertion here is uuid divergence above, not a file count: 0 records
	// is the CORRECT on-disk state for this control, unlike C1e's main case.
	agents := dirSnapshot(t, filepath.Join(home, ".loto", "agents"))
	if len(agents) != 0 {
		t.Errorf("control: agents dir holds %d records, want 0 (ephemeral identities are never persisted): %v", len(agents), agents)
	}
}
