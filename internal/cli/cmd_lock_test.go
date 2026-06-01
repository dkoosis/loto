package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"loto/internal/identity"
)

// initBareGitRepo runs `git init` and the minimal user.email/user.name config
// in `dir`. Shared by withTempProject and any test that builds an auxiliary
// repo (e.g., TestLoadCheckTargets_UsesRepoTopForGitDiff).
func initBareGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// withTempProject sets up a fresh git repo, a shared LOTO_BASE state dir, and a
// fresh HOME. Returns repoTop. Caller can use newAgent() to get a new identity.
func withTempProject(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	stateBase := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("LOTO_BASE", stateBase)
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	repo := t.TempDir()
	initBareGitRepo(t, repo)
	cmd := exec.Command("git", "remote", "add", "origin", "git@github.com:test/proj.git")
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git remote add: %v\n%s", err, out)
	}
	t.Chdir(repo)
	// Standard target files used across CLI tests. AcquireLocks Lstat-validates
	// KindFile targets, so these must exist on disk.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	storeDir := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(storeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storeDir, "store.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// pinAgent creates a fresh persisted identity in the current HOME and pins
// LOTO_AGENT_ID to it. Subsequent Run() calls will reuse this identity until
// LOTO_AGENT_ID is reset. Uses a unique CLAUDE_CODE_SESSION_ID to mint via the
// session-cache path so the identity is written to disk (loadByUUID can find
// it on subsequent Ensure calls).
func pinAgent(t *testing.T) *identity.Agent {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("pin-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", a.UUID)
	return a
}

var pinCounter atomic.Int64

func TestLockHappyPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcFlagTTL, "10m", tcFlagIntent, tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected ✓ locked: %q", out.String())
	}
}

// twoAgents creates two agents in the shared HOME (simulating two sessions on
// the same host) and returns them.
func twoAgents(t *testing.T) (alice, bob *identity.Agent) {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("alice-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("bob-%d-%d", time.Now().UnixNano(), pinCounter.Add(1)))
	b, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestLockConflictBetweenAgents(t *testing.T) {
	withTempProject(t)
	alice := pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice initial lock failed, exit %d", code)
	}

	// Second agent in the same HOME.
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("expected exit 1, got %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "✗ blocked") || !strings.Contains(combined, "blocker=") {
		t.Errorf("expected blocker report: %q", combined)
	}
	_ = alice
}

func TestLock_MultiTarget_HappyPath(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcTargetB, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d; out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "✓ locked count=2\n") {
		t.Errorf("missing triage line: %q", out.String())
	}
	for _, n := range []string{tcTargetA, tcTargetB} {
		st, err := os.Stat(filepath.Join(repo, n))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s not stripped: %o", n, st.Mode().Perm())
		}
	}
}

func TestCmdLock_SharedFlag_AllowsCoexist(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "read", "--shared"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice shared lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", "read", "--shared"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("second shared lock should succeed; code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestCmdLock_DefaultExclusive_Blocks(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", "write"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice exclusive lock failed, exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	pinAgent(t) // bob
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", "write"}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("default exclusive should block; code=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

func TestLock_RejectDirectoryTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "internal/store", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "not-regular-file") {
		t.Errorf("expected reason=not-regular-file: %q", combined)
	}
}

func TestLock_RejectNonExistentTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "missing.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "not-found") {
		t.Errorf("expected reason=not-found: %q", combined)
	}
}

func TestLock_RejectsDuplicateTargets(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, tcTargetA, "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "duplicate-target") {
		t.Errorf("expected duplicate-target: %q", combined)
	}
}

func TestLock_RejectsSymlinks(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.Symlink(filepath.Join(repo, tcTargetA), filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "link.go", "-t", tcIntentTest}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("exit %d, want 2; out=%q err=%q", code, out.String(), errBuf.String())
	}
	combined := out.String() + errBuf.String()
	if !strings.Contains(combined, "symlink") {
		t.Errorf("expected reason=symlink: %q", combined)
	}
}

// loto-dvx: parity with check (loto-d3l). `loto lock /abs/path` for a file
// inside the repo must succeed instead of being rejected as repo-escape.
func TestLock_AcceptsAbsolutePathInsideRepo(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	abs := filepath.Join(repo, tcTargetA)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, abs, "-t", tcIntentTest}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("exit %d, out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected ✓ locked: %q", out.String())
	}
}

func TestUnlockOwner(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, "-t", tcIntentDone}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("unlock exit %d; err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ unlocked") {
		t.Errorf("expected ✓ unlocked: %q", out.String())
	}
}

// TestAcquireReclaimsExpiredHolder_NoDoctor pins loto-k5el.1 SC1: after a
// holder's TTL has lapsed, a second agent acquires the same target with NO
// intervening `loto doctor`. Mechanism: AcquireLocks→reclaimStaleAndCollectBlockers.
//
// Not TDD — the acquire-time reclaim mechanism already ships; this test passes
// on first write and pins SC1 against regression.
//
// Harness note (Task 0): there is no pinAgentAs helper. Two agents are driven by
// re-pinning: pinAgent mints+pins a fresh identity, and re-pinning swaps the
// active LOTO_AGENT_ID (the same pattern TestLockConflictBetweenAgents uses).
func TestAcquireReclaimsExpiredHolder_NoDoctor(t *testing.T) {
	withTempProject(t)

	// Agent A locks with an already-expired TTL and the PID-0 sentinel (no
	// LOTO_PID), so liveness degrades to TTL and the lock is born stale.
	t.Setenv("LOTO_PID", "") // force pidUnset → PID-0 sentinel, TTL-only liveness
	pinAgent(t)              // agent A
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagTTL, "-1s"},
		&bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice initial lock failed")
	}

	// Agent B acquires the same target. No doctor run between.
	pinAgent(t) // agent B (re-pin swaps active identity)
	var out, errb bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &out, &errb)
	if code != 0 {
		t.Fatalf("bob acquire over expired holder should succeed, got exit %d: out=%q err=%q",
			code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "✓ locked") {
		t.Errorf("expected success glyph in acquire output: %q", out.String())
	}
}
