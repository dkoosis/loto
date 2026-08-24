package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/domain"
)

const (
	tcDirFerret    = "cmd/ferret"
	tcFileMain     = "cmd/ferret/main.go"
	tcFileParallel = "cmd/ferret/parallel.go"
	tcMsgRegister  = "cmd/ferret: register command"
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
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-1", tcFlagBase, base, "-m", tcMsgRegister, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "⚠ lane-unlisted-new count=1") {
		t.Errorf("missing sibling-warning triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "target="+tcFileParallel) {
		t.Errorf("warning does not name %s: %q", tcFileParallel, out.String())
	}
	if !strings.Contains(out.String(), "reason=untracked-sibling-not-in-write-set") {
		t.Errorf("row must say untracked for a never-`git add`-ed file: %q", out.String())
	}
	if strings.Contains(out.String(), "reason=staged-new-sibling-not-in-write-set") {
		t.Errorf("row must not say staged for a file that was never `git add`-ed: %q", out.String())
	}
}

// TestLane_WarnsStagedNewSiblingAndNamesItStaged is dk's #286 follow-up: a
// STAGED sibling (`git add`-ed but uncommitted) must be reported as staged,
// not flattened to the same "untracked" wording an untracked sibling gets.
// `git status` on this file shows "A  cmd/ferret/parallel.go" — a row that
// still said "untracked" would send a reader who checks that status chasing
// a mismatch instead of the actual fix (list it; no staging needed, unlike
// the untracked case). Guards against the staged/untracked rows drifting
// back together.
func TestLane_WarnsStagedNewSiblingAndNamesItStaged(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo, tcFileParallel), []byte("package main\n\nfunc ParallelCmd() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The one-line defeat codex #286 finding 1 pins: `git add` on the leftover
	// must not change what the row says beyond naming it staged instead.
	if out, err := exec.Command("git", "-C", repo, "add", tcFileParallel).CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", tcFileParallel, err, out)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-staged", tcFlagBase, base, "-m", tcMsgRegister, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "⚠ lane-unlisted-new count=1") {
		t.Errorf("missing sibling-warning triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "reason=staged-new-sibling-not-in-write-set") {
		t.Errorf("row must name the sibling as staged, not untracked: %q", out.String())
	}
	if strings.Contains(out.String(), "reason=untracked-sibling-not-in-write-set") {
		t.Errorf("row must not say untracked for a `git add`-ed file: %q", out.String())
	}
}

// TestLane_WarnsSiblingCommittedAfterBase is dk's #286 follow-up on commit
// 145a550 (Codex, sibling.go:95): `git status` reports relative to HEAD, not
// to --base. A checkout AHEAD of --base is ordinary usage — parallel.go here
// lands as a normal, fully committed change on HEAD, so `git status` shows it
// clean; the round-2 fix (untracked+staged only) would have missed it
// entirely. It is still absent from `base`'s tree, so the lane commit (built
// from base, not HEAD) omits it exactly like the untracked/staged cases.
func TestLane_WarnsSiblingCommittedAfterBase(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(repo, tcFileParallel), []byte("package main\n\nfunc ParallelCmd() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Commit ONLY parallel.go to HEAD, ahead of base — main.go (locked, about
	// to be the lane's write-set) stays uncommitted on disk. `git add -A`
	// would sweep main.go in too, so add the one file by name.
	if out, err := exec.Command("git", "-C", repo, "add", tcFileParallel).CombinedOutput(); err != nil {
		t.Fatalf("git add %s: %v\n%s", tcFileParallel, err, out)
	}
	if out, err := exec.Command("git", "-C", repo, "commit", "-qm", "feat: add parallel.go (lands on HEAD, not on base)").CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-ahead", tcFlagBase, base, "-m", tcMsgRegister, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 0 {
		t.Fatalf("lane exit %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.Contains(out.String(), "⚠ lane-unlisted-new count=1") {
		t.Errorf("missing sibling-warning triage line: %q", out.String())
	}
	if !strings.Contains(out.String(), "reason=committed-after-base-sibling-not-in-write-set") {
		t.Errorf("row must name the sibling as committed-after-base: %q", out.String())
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
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-build", tcFlagBase, base, "-m", tcMsgRegister, tcFlagCloses, tcClosesNone, "--build"}, &out, &errB)
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

// TestLane_TaintedReportSuppressesSiblingWarning pins codex #286 finding 3:
// the post-commit lock re-assertion must run BEFORE the sibling scan, not
// after. The scan shells to `git status`, which can burn up to gitTimeout on
// a wedged repo; sampling lock state only after that window would let a lease
// that expired DURING the scan report lane-tainted for a transition that
// provably could not have touched the recorded tree. This test proves the
// ORDER, not the timing (a synchronous lock-drop is enough): with an
// untracked sibling present AND a lock lost mid-flight, the output must show
// ONLY the tainted report — no ⚠ lane-unlisted-new line. Before the fix (scan
// first, reassert after), both lines appeared; the operator's actual next
// move here is to discard the tainted ref, making an advisory about its
// contents moot.
func TestLane_TaintedReportSuppressesSiblingWarning(t *testing.T) {
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
	// The untracked sibling the scan WOULD warn about, if it ran.
	if err := os.WriteFile(filepath.Join(repo, tcFileParallel), []byte("package main\n\nfunc ParallelCmd() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate a peer reclaiming the lock between the pre-assert and staging —
	// same seam TestLane_PostAssertCatchesLostLock (cmd_lane_test.go) uses.
	laneAfterPreAssert = func(rt *runtime) {
		tgt, err := domain.Canonicalize(tcFileMain)
		if err != nil {
			t.Errorf("hook canonicalize: %v", err)
			return
		}
		if _, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{tgt}, domain.AgentUUID(rt.Agent.UUID), rt.liveProbe()); err != nil {
			t.Errorf("hook release: %v", err)
		}
	}
	defer func() { laneAfterPreAssert = nil }()

	var out, errB bytes.Buffer
	code := Run([]string{tcCmdLane, tcFileMain, tcFlagRef, "ferret-taint", tcFlagBase, base, "-m", tcMsgRegister, tcFlagCloses, tcClosesNone}, &out, &errB)
	if code != 1 {
		t.Fatalf("lane exit %d, want 1 (tainted); out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✗ lane-tainted ") {
		t.Errorf("missing tainted triage line: %q", out.String())
	}
	if strings.Contains(out.String(), "lane-unlisted-new") {
		t.Errorf("sibling scan ran despite the commit being tainted (ordering regression): %q", out.String())
	}
}
