package cli

import (
	"bytes"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/identity"
)

// withTempProject sets up a fresh git repo, a shared LOTO_BASE state dir, and a
// fresh HOME. Returns repoTop. Caller can use newAgent() to get a new identity.
func withTempProject(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	stateBase := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("LOTO_BASE", stateBase)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOTO_AGENT_ID", "")

	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"remote", "add", "origin", "git@github.com:test/proj.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Chdir(repo)
	return repo
}

// pinAgent creates a fresh identity in the current HOME and pins
// LOTO_AGENT_ID to it. Subsequent Run() calls will reuse this identity until
// LOTO_AGENT_ID is reset.
func pinAgent(t *testing.T) *identity.Agent {
	t.Helper()
	t.Setenv("LOTO_AGENT_ID", "")
	a, err := identity.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", a.UUID)
	return a
}

func TestLockHappyPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, tcTargetA, "--ttl", "10m", tcFlagIntent, tcIntentTest}, &out, &errBuf)
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
	t.Setenv("LOTO_AGENT_ID", "")
	a, err := identity.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	b, err := identity.Ensure()
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
