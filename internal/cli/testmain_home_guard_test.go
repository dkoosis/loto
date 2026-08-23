package cli

import (
	"fmt"
	"os"
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
	code := m.Run()
	_ = os.RemoveAll(fallback) // best-effort; a leftover empty tmp dir is not the residue this bead is about
	os.Exit(code)
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
