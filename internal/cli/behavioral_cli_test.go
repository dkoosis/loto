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

func TestRunBehavior_HelpVerbsShowUsageNoErrorExit0(t *testing.T) {
	for _, verb := range []string{"help", "-h", "--help"} {
		t.Run(verb, func(t *testing.T) {
			stdout, stderr, code := executeCommand(verb)
			if code != 0 {
				t.Fatalf("expected exit code 0, got %d", code)
			}
			if strings.Contains(stdout, "unknown command") || strings.Contains(stderr, "unknown command") {
				t.Fatalf("expected no unknown-command line, got stdout=%q stderr=%q", stdout, stderr)
			}
			if stderr != "" {
				t.Fatalf("expected empty stderr, got %q", stderr)
			}
			if !strings.Contains(stdout, "usage: loto <command> [args]") {
				t.Fatalf("expected usage in stdout, got %q", stdout)
			}
		})
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

// TestRunBehavior_StatusOutsideRepoReturnsActionableError pins the openRuntime
// gate's error text (loto-7wi): repo-scoped commands still hard-fail outside a
// git repo — there's no rendezvous point to derive a project slug from — but
// the message must name its own remedy per design.md's actionable-finding
// convention, not just echo the raw git exec error.
func TestRunBehavior_StatusOutsideRepoReturnsActionableError(t *testing.T) {
	t.Chdir(t.TempDir())
	pinAgent(t)

	stdout, stderr, code := executeCommand(tcCmdStatus)
	if code != 3 {
		t.Fatalf("expected exit code 3, got %d", code)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "✗ not in a git repo") || !strings.Contains(stderr, "fix: git init") {
		t.Fatalf("expected actionable not-in-a-git-repo error, got %q", stderr)
	}
}

// TestRunBehavior_StatusRealGitFailureIsNotMisreportedAsNotInRepo (adversarial
// review on loto-7wi): a git failure that ISN'T "not a repository" — here,
// simulated by breaking PATH so the git binary can't be found — must not
// collapse into errNotInGitRepo's "git init" remedy. That would send an
// operator toward the wrong fix when the repo exists and it's git access
// itself that's broken (missing binary, timeout, permission).
func TestRunBehavior_StatusRealGitFailureIsNotMisreportedAsNotInRepo(t *testing.T) {
	repo := t.TempDir()
	initBareGitRepo(t, repo)
	t.Chdir(repo)
	pinAgent(t)
	t.Setenv("PATH", t.TempDir()) // git binary now unresolvable

	stdout, stderr, code := executeCommand(tcCmdStatus)
	if code != 3 {
		t.Fatalf("expected exit code 3, got %d (stderr=%q)", code, stderr)
	}
	if stdout != "" {
		t.Fatalf("expected empty stdout, got %q", stdout)
	}
	if strings.Contains(stderr, "not in a git repo") || strings.Contains(stderr, "fix: git init") {
		t.Fatalf("a real git/infra failure must not be misreported as not-in-a-repo: %q", stderr)
	}
	if !strings.Contains(stderr, "git rev-parse --show-toplevel:") {
		t.Fatalf("expected the wrapped underlying git error, got %q", stderr)
	}
}
