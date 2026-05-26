package cli

import (
	"bytes"
	"os"
	"os/exec"
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

// Negative case for normalizeRepoPath: an absolute path that does not lie
// inside the repo must still be rejected as repo-escape (no silent acceptance).
func TestCheckRejectsAbsolutePathOutsideRepo(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdCheck, "/etc/hosts"}, &out, &bytes.Buffer{})
	if code != 2 {
		t.Fatalf("expected exit 2, got %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ invalid") || !strings.Contains(out.String(), "/etc/hosts") {
		t.Errorf("expected invalid report citing /etc/hosts: %q", out.String())
	}
}

// loto-jff (gh#128): `loto check --staged` must run `git diff --cached`
// with cmd.Dir = repoTop so the staged diff comes from the loto-resolved
// repo, not from process cwd. Without the fix, when cwd is outside the
// target repo (worktree handoff, scripted invocation from a tools dir,
// nested launches), the git invocation reads the wrong repo's index and
// silently emits the wrong paths.
//
// This pins loadCheckTargets at the unit level: it must accept repoTop
// and pass it to git, independent of process cwd.
func TestLoadCheckTargets_UsesRepoTopForGitDiff(t *testing.T) {
	// repoA: the target repo with a staged file. Built by hand so the test
	// is independent of withTempProject side effects (cwd/env).
	repoA := t.TempDir()
	initBareGitRepo(t, repoA)
	stagedRel := filepath.Join("internal", "store", "store.go")
	if err := os.MkdirAll(filepath.Join(repoA, "internal", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoA, stagedRel), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "add", stagedRel)
	cmd.Dir = repoA
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("repoA git add: %v\n%s", err, out)
	}

	// cwd points elsewhere (a non-git directory). Without the fix,
	// loadCheckTargets would inherit this cwd and `git diff --cached` would
	// fail (or read whatever ambient repo it discovers).
	cwd := t.TempDir()
	t.Chdir(cwd)

	var stderr bytes.Buffer
	paths, code := loadCheckTargets(t.Context(), repoA, true, nil, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d (stderr=%q)", code, stderr.String())
	}
	if len(paths) != 1 || filepath.ToSlash(paths[0]) != filepath.ToSlash(stagedRel) {
		t.Fatalf("expected staged path %q, got %v", stagedRel, paths)
	}
}
