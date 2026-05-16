package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckClean(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "no conflicts") && !strings.Contains(out.String(), "no paths") {
		t.Errorf("expected clean output: %q", out.String())
	}
}

func TestCheckConflictsWithOtherAgent(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, tcTargetA}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ conflicts") || !strings.Contains(out.String(), "blocker=") {
		t.Errorf("expected conflict report: %q", out.String())
	}
}

// loto-d3l: absolute path that lies inside the repo must report the same
// conflict as the equivalent repo-relative path. Previously the CLI swallowed
// ErrRepoEscape from Canonicalize and emitted "✓ no conflicts".
func TestCheckAcceptsAbsolutePathInsideRepo(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)

	abs := filepath.Join(repo, tcTargetA)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, abs}, &out, &bytes.Buffer{})
	if code != 1 {
		t.Fatalf("expected exit 1, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ conflicts") || !strings.Contains(out.String(), "blocker=") {
		t.Errorf("expected conflict report for abs path: %q", out.String())
	}
}
