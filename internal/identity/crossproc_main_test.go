//go:build unix

// Cross-process contract-test harness spine (loto-qhv build-order item 5).
// Same shape as internal/store/crossproc_main_test.go (item 1): TestMain
// intercepts LOTO_CROSSPROC_ROLE and, when set, dispatches to a role
// function instead of running the normal test suite — the same
// race-instrumented test binary re-execs itself as a child. See the
// loto-qhv plan
// (~/Projects/dk/Project/loto/plans/loto-qhv-crossproc-contract-tests.md)
// §2 for the full design, and §2.2's "duplicated per package, ~90 lines
// each" call: this file intentionally does NOT import internal/store — the
// two spines are textually divergent twins, not a shared helper (arch-lint
// pins internal/identity → ∅).
//
// This file carries only the spine — TestMain, the role table, spawnChild,
// the pipe barrier, the JSON verdict, process-group cleanup, and the child
// watchdog — plus one smoke test proving the mechanism, mirroring store's
// PR-1 split exactly: a future family's role functions live in that family's
// own file and only add rows to crossProcRoles below, never a second
// TestMain.
package identity

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// crossProcRoleEnv is the env var TestMain checks to decide whether this
// process invocation is a normal `go test` run or a re-exec'd child playing
// one role in a cross-process contract test.
const crossProcRoleEnv = "LOTO_CROSSPROC_ROLE"

// crossProcWatchdog bounds how long an orphaned child can survive a crashed
// or SIGKILLed parent. Sized ~100x the expected per-role duration (plan §5).
const crossProcWatchdog = 20 * time.Second

// crossProcOutcome is the closed set of outcomes a child reports in its JSON
// verdict line.
type crossProcOutcome string

const (
	crossProcWon      crossProcOutcome = "won"
	crossProcConflict crossProcOutcome = "conflict"
	crossProcError    crossProcOutcome = "error"
	crossProcTimedOut crossProcOutcome = "watchdog"
)

// Exit codes mirror crossProcOutcome 1:1 so the parent can classify a child
// from its process exit status alone, without needing stdout to be well
// formed.
const (
	exitWon      = 0
	exitConflict = 1
	exitError    = 2
	exitWatchdog = 3
)

// crossProcVerdict is the one JSON line a child role emits on stdout before
// exiting. encoding/json only — stdlib, per the package's no-assertion-
// helper-deps convention (#176). Owner carries the resolved agent uuid for
// every identity role — there is no cross-process typed-error case here
// (unlike store's Family A), so the shape is trimmed relative to store's
// twin: no Blockers, no ProcStart (Family C2 is out of this bead's scope).
type crossProcVerdict struct {
	Role    string           `json:"role"`
	Outcome crossProcOutcome `json:"outcome"`
	Owner   string           `json:"owner,omitempty"`
	PID     int              `json:"pid"`
	PPID    int              `json:"ppid"`
	Err     string           `json:"err,omitempty"`
}

// crossProcRoleSmokeName is this file's one role.
const crossProcRoleSmokeName = "smoke"

// crossProcRoles is the role dispatch table. This file's spine owns exactly
// one row (smoke). Families C1 (identity stability across ppid/exec/cwd
// churn) and C2 (peer liveness) were retired with the registry they tested
// (loto-jnid): the owner is now the session id read from env, so there is no
// on-disk binding whose stability a child process could contradict. A future
// family adds its rows here rather than opening a second TestMain.
var crossProcRoles = map[string]func() crossProcVerdict{
	crossProcRoleSmokeName: crossProcRoleSmoke,
}

// crossProcRoleSmoke reports its own pid/ppid after clearing the barrier —
// the one fact TestCrossProc_Smoke needs to prove a real child process, in
// its own process group, under the real parent, actually ran. Deliberately
// identity-agnostic (no call to Ensure) so a harness break and an identity
// break produce distinguishable failures.
func crossProcRoleSmoke() crossProcVerdict {
	const role = crossProcRoleSmokeName
	if err := crossProcAwaitBarrier(); err != nil {
		return crossProcVerdict{Role: role, Outcome: crossProcError, PID: os.Getpid(), PPID: os.Getppid(), Err: err.Error()}
	}
	return crossProcVerdict{Role: role, Outcome: crossProcWon, PID: os.Getpid(), PPID: os.Getppid()}
}

// crossProcAwaitBarrier is the child side of the pipe barrier a parent sets
// up via newCrossProcBarrier: signal ready by writing one byte to fd 4, then
// block reading fd 3 until every child's read returns EOF at once (the
// parent closes the shared write end). Fds 3/4 are the two crossProcBarrier
// pipe ends, handed to spawnChild as extra files in that order.
func crossProcAwaitBarrier() error {
	readyW := os.NewFile(4, "crossproc-ready")
	if _, err := readyW.Write([]byte{1}); err != nil {
		return fmt.Errorf("signal ready (fd 4): %w", err)
	}
	if err := readyW.Close(); err != nil {
		return fmt.Errorf("close ready fd: %w", err)
	}

	startR := os.NewFile(3, "crossproc-start")
	defer startR.Close()
	buf := make([]byte, 1)
	if _, err := startR.Read(buf); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("await start (fd 3): %w", err)
	}
	return nil
}

// TestMain intercepts LOTO_CROSSPROC_ROLE before any *testing.T exists. Set
// → this process is a re-exec'd child: arm the watchdog, dispatch to the
// named role, exit with its verdict's code. Unset → a normal `go test` run;
// TestMain never calls m.Run() (and so never parses testing flags) on the
// child path, which is why spawnChild needs no -test.* arguments.
//
// The normal-run branch also installs loto-bt6c's structural guard: this
// package owns the session-record dir, so every one of its own tests that
// forgets its own HOME redirect (t.Setenv("HOME", t.TempDir())) would write
// straight into dk's real ~/.loto/session — a session record written there and
// every `loto` invocation's tax the bead describes. Repointing HOME here,
// once, before any test runs, means an omission lands in a throwaway
// directory instead. Individual tests keep their own per-test t.Setenv for
// isolation FROM EACH OTHER; this is only the floor under all of them. A
// re-exec'd child (the role branch above) is untouched — it must keep the
// exact curated env its parent test built for it.
func TestMain(m *testing.M) {
	if role := os.Getenv(crossProcRoleEnv); role != "" {
		os.Exit(runCrossProcRole(role))
	}
	fallback, err := os.MkdirTemp("", "loto-identity-testhome-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: mkdir fallback HOME:", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", fallback); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: set fallback HOME:", err)
		os.Exit(1)
	}
	code := m.Run()
	_ = os.RemoveAll(fallback)
	os.Exit(code)
}

// runCrossProcRole arms the child self-watchdog — an orphan from a crashed
// or SIGKILLed parent (deliberately so for C1a's "G") must not outlive the
// run (plan §2.2) — then dispatches role to its function and emits its
// verdict. An unknown role is a harness bug, not a test failure mode: it
// exits loudly (exitError) rather than silently no-op'ing, since a typo'd
// role would otherwise hang the parent's wait forever.
func runCrossProcRole(role string) int {
	timer := time.AfterFunc(crossProcWatchdog, func() {
		os.Exit(emitVerdict(crossProcVerdict{
			Role:    role,
			Outcome: crossProcTimedOut,
			PID:     os.Getpid(),
			PPID:    os.Getppid(),
			Err:     "watchdog fired: role did not complete in time",
		}))
	})
	defer timer.Stop()

	fn, ok := crossProcRoles[role]
	if !ok {
		fmt.Fprintf(os.Stderr, "crossproc: unknown role %q\n", role)
		return exitError
	}
	return emitVerdict(fn())
}

// emitVerdict writes v as one JSON line to stdout and returns the exit code
// matching v.Outcome.
func emitVerdict(v crossProcVerdict) int {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		// Nothing further can signal the parent reliably at this point;
		// exit with an error code so the parent's JSON decode fails loudly
		// instead of hanging on a verdict line that never arrives.
		fmt.Fprintf(os.Stderr, "crossproc: encode verdict: %v\n", err)
		return exitError
	}
	switch v.Outcome {
	case crossProcWon:
		return exitWon
	case crossProcConflict:
		return exitConflict
	case crossProcTimedOut:
		return exitWatchdog
	case crossProcError:
		return exitError
	default:
		return exitError
	}
}

// child wraps one spawned cross-process role invocation together with the
// plumbing needed to reap it deterministically and decode its verdict.
type child struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// spawnChild starts role as a re-exec of the current test binary. dir, if
// non-empty, sets the child's working directory (C1c's cwd-churn case);
// empty leaves it at the harness process's own cwd. env is merged into a
// curated (not inherited-plus) child environment — the whole point for
// HOME/PPID-churn tests, since it means these children never depend on
// what the parent's own env happened to contain. extra files are handed to
// the child as ExtraFiles, landing at fd 3, 4, … in that order — the
// crossProcBarrier pipe ends, for tests that use one.
func spawnChild(t *testing.T, role, dir string, env map[string]string, extra ...*os.File) *child {
	t.Helper()
	return spawnChildArgs(t, role, dir, env, nil, extra...)
}

// spawnChildArgs is spawnChild with control over the child's argv tail. The
// role path in TestMain exits before m.Run(), so testing never parses these
// arguments — they exist purely so a child's REAL command line can carry
// flags a role reads back out of the proc table. Callers that don't need an
// argv tail use spawnChild.
func spawnChildArgs(t *testing.T, role, dir string, env map[string]string, args []string, extra ...*os.File) *child {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("crossproc: resolve test binary: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), exe, args...)
	cmd.Dir = dir
	cmd.Env = crossProcChildEnv(role, env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.ExtraFiles = extra
	cmd.WaitDelay = 5 * time.Second

	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	if err := cmd.Start(); err != nil {
		t.Fatalf("crossproc: start child role=%s: %v", role, err)
	}

	c := &child{t: t, cmd: cmd, stdout: &out, stderr: &errOut}

	// Process-group cleanup: if the test ends (pass, fail, or fatal) before
	// wait() has reaped this child, kill the whole process group and reap
	// it here so no orphan survives the test.
	t.Cleanup(func() {
		if cmd.Process == nil || cmd.ProcessState != nil {
			return
		}
		if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
		_ = cmd.Wait()
	})

	return c
}

// crossProcChildEnv builds the child's environment explicitly — role, a
// usable PATH, and whatever HOME/session/agent-id vars the test supplies
// through extra. Never t.Setenv, so unlike internal/identity/registry_test.go
// these tests impose no t.Parallel restriction on the package (plan §2.2
// surprise log).
func crossProcChildEnv(role string, extra map[string]string) []string {
	env := make([]string, 0, 2+len(extra))
	env = append(env,
		crossProcRoleEnv+"="+role,
		"PATH="+os.Getenv("PATH"),
	)
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

// wait reaps the child and decodes its JSON verdict line. Safe to call at
// most once per child.
func (c *child) wait() (crossProcVerdict, error) {
	c.t.Helper()
	waitErr := c.cmd.Wait()

	var v crossProcVerdict
	if decErr := json.Unmarshal(bytes.TrimSpace(c.stdout.Bytes()), &v); decErr != nil {
		waitMsg := "<nil>"
		if waitErr != nil {
			waitMsg = waitErr.Error()
		}
		return v, fmt.Errorf("crossproc: decode verdict: %w (wait=%s stdout=%q stderr=%q)",
			decErr, waitMsg, c.stdout.String(), c.stderr.String())
	}
	return v, waitErr
}

// crossProcBarrier is the kernel-mediated release-N-processes-at-once
// primitive cross-process contract tests use instead of a sleep (plan
// §2.2). readyR/readyW: each child writes one byte when it reaches the
// barrier; the parent blocks reading exactly N bytes via awaitReady. startR/
// startW: startR is handed to each child as fd 3; every child blocks on a
// 1-byte Read; release closes startW so every child's Read returns EOF at
// the same instant.
type crossProcBarrier struct {
	readyR, readyW *os.File
	startR, startW *os.File
}

// newCrossProcBarrier allocates both pipes and registers cleanup for
// whichever ends the test doesn't already close itself (release() closes
// startW; readyR/readyW/startR are always left for cleanup).
func newCrossProcBarrier(t *testing.T) *crossProcBarrier {
	t.Helper()
	readyR, readyW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: ready pipe: %v", err)
	}
	startR, startW, err := os.Pipe()
	if err != nil {
		t.Fatalf("crossproc: start pipe: %v", err)
	}
	b := &crossProcBarrier{readyR: readyR, readyW: readyW, startR: startR, startW: startW}
	t.Cleanup(func() {
		_ = readyR.Close()
		_ = readyW.Close()
		_ = startR.Close()
		_ = startW.Close() // idempotent-in-effect: release() may have closed this already; the second Close's error is discarded
	})
	return b
}

// startFile is fd 3 in every child spawned against this barrier.
func (b *crossProcBarrier) startFile() *os.File { return b.startR }

// readyFile is fd 4 in every child spawned against this barrier — safe to
// hand the same *os.File to multiple children; exec dups it per child.
func (b *crossProcBarrier) readyFile() *os.File { return b.readyW }

// awaitReady blocks until n children have each written one byte to the
// ready pipe.
func (b *crossProcBarrier) awaitReady(t *testing.T, n int) {
	t.Helper()
	buf := make([]byte, n)
	if _, err := io.ReadFull(b.readyR, buf); err != nil {
		t.Fatalf("crossproc: await ready (want %d): %v", n, err)
	}
}

// release lets every parked child's fd-3 Read return EOF at the same
// instant.
func (b *crossProcBarrier) release() {
	_ = b.startW.Close()
}

// TestCrossProc_Smoke proves the harness mechanism itself — role dispatch,
// the pipe barrier, fd inheritance across exec, and process-group cleanup —
// before any contract logic runs; nothing here asserts anything about
// identity.
func TestCrossProc_Smoke(t *testing.T) {
	if testing.Short() {
		t.Skip("crossproc: cross-process harness skipped under -short")
	}

	b := newCrossProcBarrier(t)
	c := spawnChild(t, crossProcRoleSmokeName, "", nil, b.startFile(), b.readyFile())

	b.awaitReady(t, 1)
	b.release()

	v, err := c.wait()
	if err != nil {
		t.Fatalf("crossproc: child wait: %v", err)
	}
	if v.Outcome != crossProcWon {
		t.Fatalf("outcome = %q, want %q (err=%q)", v.Outcome, crossProcWon, v.Err)
	}
	if v.PPID != os.Getpid() {
		t.Errorf("ppid = %d, want parent pid %d", v.PPID, os.Getpid())
	}
	if v.PID == os.Getpid() {
		t.Errorf("pid = %d, want a distinct child pid", v.PID)
	}
	if v.PID <= 0 {
		t.Errorf("pid = %d, want a positive pid", v.PID)
	}

	// Reaped: cmd.Wait() above already returned, so the child's process
	// group should have no live members left.
	if err := syscall.Kill(-c.cmd.Process.Pid, 0); err == nil {
		t.Errorf("process group %d still has live members after wait", c.cmd.Process.Pid)
	}
}

// realHomeAtProcessStart captures $HOME exactly as the test binary inherited
// it, before TestMain's loto-bt6c guard above ever touches the env var —
// var initializers run before TestMain, so this is the one legitimate read
// of the pre-override value anywhere in the package.
var realHomeAtProcessStart = os.Getenv("HOME")

// TestHomeGuardCanary_HomeIsNeverTheRealOne is the regression test for the
// loto-bt6c floor in TestMain: it makes no HOME redirect of its own, so it
// only passes because TestMain already repointed HOME before this — or any
// other — test in the package got to run. Weaken or delete that guard and
// this goes red immediately instead of the leak silently resuming.
func TestHomeGuardCanary_HomeIsNeverTheRealOne(t *testing.T) {
	if realHomeAtProcessStart == "" {
		t.Skip("no real $HOME in this environment to compare against")
	}
	if got := os.Getenv("HOME"); got == realHomeAtProcessStart {
		t.Fatalf("HOME = %q, want anything but the real invoking-user home %q — the loto-bt6c isolation floor is not active", got, realHomeAtProcessStart)
	}
}
