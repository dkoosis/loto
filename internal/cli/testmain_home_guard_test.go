package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// realHomeAtProcessStart captures $HOME exactly as the test binary inherited
// it, before TestMain below ever touches the env var. Every other test in
// this package reads $HOME only after its own (or withTempProject's)
// t.Setenv("HOME", ...) has run, so this is the one place in the package
// that legitimately needs the pre-override value — testHomeGuardCanary uses
// it to prove the floor below is actually in effect.
var realHomeAtProcessStart = os.Getenv("HOME")

// TestMain is loto-bt6c's structural backstop: three tests in this package
// (TestRunBehavior_CheckStagedOutsideRepoReturnsError and its two siblings)
// minted agent identities into dk's real ~/.loto because they called
// pinAgent without first redirecting HOME — that's how the pin-<nanos>-<n>
// residue in the bead got there. Per-test isolation (withTempProject,
// t.Setenv("HOME", t.TempDir())) is still the norm and stays in every test
// that needs it; this is the floor underneath it. Before any test in this
// binary runs, HOME is repointed at a throwaway directory — a future test
// that forgets its own redirect lands there, never in the real home. The
// only way to defeat this floor is to explicitly restore the real HOME
// inside a test, which is a conspicuous act, not an oversight.
func TestMain(m *testing.M) {
	fallback, err := os.MkdirTemp("", "loto-cli-testhome-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: mkdir fallback HOME:", err)
		os.Exit(1)
	}
	if err := os.Setenv("HOME", fallback); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: set fallback HOME:", err)
		os.Exit(1)
	}
	template, err := os.MkdirTemp("", "loto-cli-git-template-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: mkdir git template:", err)
		os.Exit(1)
	}
	if err := buildGitRepoTemplate(template); err != nil {
		fmt.Fprintln(os.Stderr, "TestMain: build git template:", err)
		os.Exit(1)
	}
	gitRepoTemplateDir = template

	code := m.Run()
	_ = os.RemoveAll(fallback) // best-effort; a leftover empty tmp dir is not the residue this bead is about
	_ = os.RemoveAll(template)
	os.Exit(code)
}

// gitRepoTemplateDir holds a `.git` directory built ONCE per test binary run
// (below) with the exact init+config sequence initBareGitRepo used to run
// per-test: `git init -q`, `git config user.email`, `git config user.name`.
// initBareGitRepo (cmd_lock_test.go) now copies this template's `.git` tree
// into each test's fresh repo dir instead of re-running those three git
// subprocesses per test. loto-a0fs measured the per-test cost as git-exec
// overhead, not compute (~300ms/exec under load) — 200+ call sites each
// spawning 3 processes was the bulk of internal/cli's wall time. A filesystem
// copy produces an identical `.git` tree with zero subprocess spawns.
var gitRepoTemplateDir string

// buildGitRepoTemplate runs the one real `git init` + config sequence that
// every withTempProject/initBareGitRepo caller used to run for itself. dir
// must be empty; on return dir/.git is a ready-to-copy template.
func buildGitRepoTemplate(dir string) error {
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
	}
	return nil
}

// copyGitTemplate copies gitRepoTemplateDir's `.git` tree into dir/.git. Used
// by initBareGitRepo (cmd_lock_test.go) in place of the git subprocess
// sequence buildGitRepoTemplate ran once above.
func copyGitTemplate(dir string) error {
	return os.CopyFS(filepath.Join(dir, ".git"), os.DirFS(filepath.Join(gitRepoTemplateDir, ".git")))
}

// TestHomeGuardCanary_HomeIsNeverTheRealOne is the regression test for
// TestMain's floor above: it makes no HOME redirect of its own, so it only
// passes because TestMain already repointed HOME before this test — or any
// other — got to run. Delete or weaken TestMain's os.Setenv("HOME", ...)
// and this goes red immediately, instead of the leak silently resuming.
func TestHomeGuardCanary_HomeIsNeverTheRealOne(t *testing.T) {
	if realHomeAtProcessStart == "" {
		t.Skip("no real $HOME in this environment to compare against")
	}
	if got := os.Getenv("HOME"); got == realHomeAtProcessStart {
		t.Fatalf("HOME = %q, want anything but the real invoking-user home %q — the loto-bt6c isolation floor is not active", got, realHomeAtProcessStart)
	}
}
