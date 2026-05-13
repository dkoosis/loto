package cli

import (
	"strings"
	"testing"
)

func TestRunBehavior_NoArgsShowsHelpAndExit2(t *testing.T) {
	stdout, stderr, code := executeCommand()
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "usage: loto <command> [args]") {
		t.Fatalf("expected usage in stderr, got %q", stderr)
	}
}

func TestRunBehavior_UnknownCommandShowsErrorAndHelp(t *testing.T) {
	stdout, stderr, code := executeCommand("nope")
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "unknown command: nope") {
		t.Fatalf("expected unknown-command error, got %q", stderr)
	}
	if !strings.Contains(stderr, "commands:") {
		t.Fatalf("expected help text in stderr, got %q", stderr)
	}
}

func TestRunBehavior_CheckInvalidFlagReturnsUsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	stdout, stderr, code := executeCommand(tcCmdCheck, "--bogus")
	if code != 2 {
		t.Fatalf("expected exit code 2, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "flag provided but not defined") {
		t.Fatalf("expected flag parse error, got %q", stderr)
	}
	if !strings.Contains(stderr, "Usage of check:") {
		t.Fatalf("expected check usage in stderr, got %q", stderr)
	}
}

func TestRunBehavior_CheckStagedOutsideRepoReturnsError(t *testing.T) {
	t.Chdir(t.TempDir())
	pinAgent(t)

	stdout, stderr, code := executeCommand(tcCmdCheck, "--staged")
	if code != 3 {
		t.Fatalf("expected exit code 3, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "git diff: exit status") {
		t.Fatalf("expected git-repo error in stderr, got %q", stderr)
	}
}
