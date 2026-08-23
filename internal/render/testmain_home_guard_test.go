package render

import (
	"fmt"
	"os"
	"testing"
)

// realHomeAtProcessStart captures $HOME exactly as the test binary inherited
// it, before TestMain below ever touches the env var — every other test in
// this package reads $HOME only after its own t.Setenv("HOME", ...) has run,
// so this is the one place that legitimately needs the pre-override value;
// testHomeGuardCanary uses it to prove the floor below is actually in
// effect.
var realHomeAtProcessStart = os.Getenv("HOME")

// TestMain is loto-bt6c's structural backstop for this package: cli_test.go
// and gate_test.go both resolve identity (holderTag → registryDir()) via
// $HOME, so a future test that forgets its own t.Setenv("HOME", ...) would
// mint straight into dk's real ~/.loto. Repointing HOME here, once, before
// any test runs, means an omission lands in a throwaway directory instead —
// per-test t.Setenv calls stay in place for isolation between tests; this is
// only the floor beneath them.
func TestMain(m *testing.M) {
	fallback, err := os.MkdirTemp("", "loto-render-testhome-*")
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

// TestHomeGuardCanary_HomeIsNeverTheRealOne is the regression test for
// TestMain's floor above: it makes no HOME redirect of its own, so it only
// passes because TestMain already repointed HOME before this — or any
// other — test in the package got to run. Delete or weaken TestMain's
// os.Setenv("HOME", ...) and this goes red immediately, instead of the leak
// silently resuming.
func TestHomeGuardCanary_HomeIsNeverTheRealOne(t *testing.T) {
	if realHomeAtProcessStart == "" {
		t.Skip("no real $HOME in this environment to compare against")
	}
	if got := os.Getenv("HOME"); got == realHomeAtProcessStart {
		t.Fatalf("HOME = %q, want anything but the real invoking-user home %q — the loto-bt6c isolation floor is not active", got, realHomeAtProcessStart)
	}
}
