package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tcDirFerret    = "cmd/ferret"
	tcFileMain     = "cmd/ferret/main.go"
	tcFileParallel = "cmd/ferret/parallel.go"
)

// lane_check_test.go is the executable spec for loto-5aug: `loto lane` commits
// a hand-listed write-set by plumbing, so the working tree an author verified
// and the tree the commit records can legitimately differ. These tests
// reproduce the incident (ferret PR #134, CI run 32609080351) end-to-end:
// cmd/ferret/main.go registered a command whose body lived in
// cmd/ferret/parallel.go, an untracked leftover from an abandoned branch; the
// lane listed main.go, not parallel.go, and the commit referenced
// ParallelCmd nowhere in its own tree. Local `make check` was green — it
// verified the working tree, which was consistent. The commit never was; CI
// failed on `undefined: ParallelCmd`.

// TestLane_WarnsUnlistedUntrackedSiblingInSamePackage is the acceptance test
// for the default (cheap, always-on) mechanism: an untracked file sharing a
// directory with a listed file triggers a ⚠ advisory, without blocking the
// commit — the AC's "warns" branch. This is the exact incident shape.
func TestLane_WarnsUnlistedUntrackedSiblingInSamePackage(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")

	if err := os.MkdirAll(filepath.Join(repo, tcDirFerret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, tcFileMain), []byte("package main\n\nfunc main() { ParallelCmd() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcFileMain, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcFileMain)
	}
	// parallel.go: the abandoned-branch leftover. Untracked, never listed — the
	// lane's write-set is [main.go] only.
	if err := os.WriteFile(filepath.Join(repo, tcFileParallel), []byte("package main\n\nfunc ParallelCmd() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-1", tcFlagBase, base, "-m", "cmd/ferret: register command", tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "⚠ lane-unlisted-new count=1") {
		t.Errorf("missing sibling-warning triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "target="+tcFileParallel) {
		t.Errorf("warning does not name %s: %q", tcFileParallel, out.String())
	}
}

// TestLane_SilentOnUnrelatedUntrackedFileElsewhere is the false-alarm guard at
// the CLI layer: an untracked file in a directory the write-set never touches
// must not warn. A false alarm on a legitimate lane is nearly as expensive as
// the miss it prevents — agents learn to ignore a noisy check.
func TestLane_SilentOnUnrelatedUntrackedFileElsewhere(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	base := commitAllInRepo(t, repo, "init")

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	if err := os.MkdirAll(filepath.Join(repo, tcDirFerret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, tcFileParallel), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcTargetA, tcFlagRef, "ferret-2", tcFlagBase, base, "-m", tcMsg, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if strings.Contains(out.String(), "lane-unlisted-new") {
		t.Errorf("unexpected sibling-warning for an unrelated directory: %q", out.String())
	}
}

// TestLane_BuildFlagCatchesUndefinedSymbolFromUnlistedFile is the acceptance
// test for the thorough (opt-in, --build) mechanism: it actually builds the
// committed ref in a throwaway worktree and refuses (exit 1) with the
// compiler's own verdict when a listed file references a symbol that exists
// only in an unlisted file. Uses its own minimal go.mod-rooted repo — a real
// `go build` needs a real module, unlike withTempProject's bare fixture.
func TestLane_BuildFlagCatchesUndefinedSymbolFromUnlistedFile(t *testing.T) {
	// HOME/state isolation must land BEFORE the first git commit — a global
	// commit-msg hook (conventional-commit enforcement) resolves against the
	// real HOME otherwise and rejects this test's throwaway "init" message.
	home := t.TempDir()
	stateBase := filepath.Join(t.TempDir(), "state")
	t.Setenv("HOME", home)
	t.Setenv("LOTO_BASE", stateBase)
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	repo := t.TempDir()
	initBareGitRepo(t, repo)
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module lanebuildtest\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	base := commitAllInRepo(t, repo, "init")

	t.Chdir(repo)
	pinAgent(t)

	if err := os.MkdirAll(filepath.Join(repo, tcDirFerret), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, tcFileMain), []byte("package main\n\nfunc main() { ParallelCmd() }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := Run([]string{tcCmdLock, tcFileMain, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcFileMain)
	}
	// parallel.go, defining ParallelCmd, is deliberately never written to disk
	// here — the lane's write-set (main.go alone) must fail to build on its own.

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-build", tcFlagBase, base, "-m", "cmd/ferret: register command", tcFlagCloses, tcClosesNone, "--build"}, &out, &errB)
	if code != 1 {
		t.Fatalf("lane --build exit %d, want 1; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "✗ lane-build-failed") {
		t.Errorf("missing build-failed triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "undefined: ParallelCmd") && !strings.Contains(out.String(), "undefined:") {
		t.Errorf("build output does not name the undefined symbol: %q", out.String())
	}
}
