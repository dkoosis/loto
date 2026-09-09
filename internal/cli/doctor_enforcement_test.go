package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// This file is enforcement-design.md §10.3's seven red conditions and the
// §11 nested-worktree advisory, as TDD (loto-ea8y.7). Rules 1 and 2 (global
// core.hooksPath ownership, reference-transaction reachability) are already
// covered by TestDoctorGuard_* in cmd_doctor_test.go via the extended
// guardSpecs table; this file covers rules 3 through 7 and the advisory.

// writeTreeHookSettings registers all three tree-hook events, correctly
// shaped, in the current HOME's ~/.claude/settings.json — one of "the
// settings files Claude Code actually loads" (this bead's Givens).
func writeTreeHookSettings(t *testing.T) {
	t.Helper()
	home := os.Getenv("HOME")
	if home == "" {
		t.Fatal("HOME unset; withTempProject must run first")
	}
	const tcSettingsCmdType = "command"
	sf := settingsFile{Hooks: map[string][]settingsMatcherGroup{
		"PreToolUse": {{
			Matcher: TreeHookMatcher,
			Hooks:   []settingsCommand{{Type: tcSettingsCmdType, Command: "loto hook pre"}},
		}},
		"PostToolUse": {{
			Matcher: TreeHookMatcher,
			Hooks:   []settingsCommand{{Type: tcSettingsCmdType, Command: "loto hook post"}},
		}},
		eventPostToolUseFailure: {{
			Matcher: TreeHookMatcher,
			Hooks:   []settingsCommand{{Type: tcSettingsCmdType, Command: "loto hook post"}},
		}},
	}}
	b, err := json.Marshal(sf)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(home, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSharedGuardFixture points a fresh, isolated GLOBAL core.hooksPath at a
// directory carrying a marker-bearing executable `pre-commit` — the shape
// checkSharedPreCommitGuard (rule 7 / R8) reads. markerLine "" omits the
// marker, producing the marker-absent red case; noPreCommit true omits the
// file entirely, producing the unreachable red case.
func writeSharedGuardFixture(t *testing.T, marked, present bool) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	dir := t.TempDir()
	if present {
		body := "#!/bin/sh\nexit 0\n"
		if marked {
			body = "#!/bin/sh\n# " + sharedGuardMarker + "\nexit 0\n"
		}
		if err := os.WriteFile(filepath.Join(dir, "pre-commit"), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	submitGitT(t, t.TempDir(), "config", "--global", "core.hooksPath", dir)
	return dir
}

// unsetGlobalHooksPath isolates GLOBAL git config to a fresh, empty file so
// core.hooksPath reads as unset regardless of the host machine's real config.
func unsetGlobalHooksPath(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
}

// sameFileSeams points both checkHookBinaryIdentity seams at one file, so the
// hashes match by construction.
func sameFileSeams(t *testing.T, path string) {
	t.Helper()
	prevLook, prevRun := lookPathLoto, runningExecutable
	lookPathLoto = func() (string, error) { return path, nil }
	runningExecutable = func() (string, error) { return path, nil }
	t.Cleanup(func() { lookPathLoto, runningExecutable = prevLook, prevRun })
}

// --- rule 3: tree-hook settings entries -------------------------------------

func TestDoctorEnforcement_TreeHookSettingsMissing(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ tree_hooks missing=PreToolUse,PostToolUse,PostToolUseFailure") {
		t.Errorf("expected the tree_hooks red row: %q", out)
	}
	if !strings.Contains(out, "```bash") {
		t.Errorf("expected a bash fix block under the ✗ row: %q", out)
	}
	// PR #344 review: the fix block must carry a copy-pasteable settings.json
	// hunk, not just a prose summary — every event, the exact matcher, and
	// the exact command each one invokes.
	for _, want := range []string{
		`"PreToolUse"`, `"PostToolUse"`, `"` + eventPostToolUseFailure + `"`,
		`"matcher": "` + TreeHookMatcher + `"`,
		`"command": "loto hook pre"`,
		`"command": "loto hook post"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected the fix block to carry %q verbatim: %q", want, out)
		}
	}
}

func TestDoctorEnforcement_TreeHookSettingsRegistered(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	writeTreeHookSettings(t)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ tree_hooks registered events=3") {
		t.Errorf("expected the tree_hooks ✓ row: %q", out)
	}
}

// --- rule 4: installed hook target's hash -----------------------------------

func TestDoctorEnforcement_HookBinaryNotOnPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	prevLook := lookPathLoto
	lookPathLoto = func() (string, error) { return "", exec.ErrNotFound }
	t.Cleanup(func() { lookPathLoto = prevLook })

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ hook_binary reason=not-on-path") {
		t.Errorf("expected the not-on-path red row: %q", out)
	}
	if !strings.Contains(out, "```bash\nmake install\n```") {
		t.Errorf("expected the make install fix block: %q", out)
	}
}

func TestDoctorEnforcement_HookBinaryHashMismatch(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	a := filepath.Join(repo, "loto-a")
	b := filepath.Join(repo, "loto-b")
	if err := os.WriteFile(a, []byte("binary-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("binary-b"), 0o755); err != nil {
		t.Fatal(err)
	}
	prevLook, prevRun := lookPathLoto, runningExecutable
	lookPathLoto = func() (string, error) { return a, nil }
	runningExecutable = func() (string, error) { return b, nil }
	t.Cleanup(func() { lookPathLoto, runningExecutable = prevLook, prevRun })

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ hook_binary reason=hash-mismatch installed="+a+" running="+b) {
		t.Errorf("expected the hash-mismatch red row naming both paths: %q", out)
	}
	if !strings.Contains(out, "```bash\nmake install\n```") {
		t.Errorf("expected the make install fix block: %q", out)
	}
}

func TestDoctorEnforcement_HookBinaryMatch(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	same := filepath.Join(repo, "loto-same")
	if err := os.WriteFile(same, []byte("one-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sameFileSeams(t, same)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ hook_binary match path="+same) {
		t.Errorf("expected the hook_binary ✓ row: %q", out)
	}
}

// --- rule 5: synthetic pre-then-post self-test ------------------------------

func TestDoctorEnforcement_SelfTestPreRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	prevPre := selfTestPre
	selfTestPre = func(context.Context, *runtime, hookEvent, time.Time, io.Writer, io.Writer) int { return 2 }
	t.Cleanup(func() { selfTestPre = prevPre })

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ hook_selftest reason=pre-refused exit=2") {
		t.Errorf("expected the pre-refused red row: %q", out)
	}
	if !strings.Contains(out, "```bash") {
		t.Errorf("expected a bash fix block under the ✗ row: %q", out)
	}
}

func TestDoctorEnforcement_SelfTestPostRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	prevPost := selfTestPost
	selfTestPost = func(context.Context, *runtime, hookEvent, time.Time, io.Writer) int { return 2 }
	t.Cleanup(func() { selfTestPost = prevPost })

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ hook_selftest reason=post-refused exit=2") {
		t.Errorf("expected the post-refused red row: %q", out)
	}
}

func TestDoctorEnforcement_SelfTestRecordsPair(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ hook_selftest pre+post recorded") {
		t.Errorf("expected the hook_selftest ✓ row: %q", out)
	}
}

// countCallsAndEvents reads the two counters PR #344 CI (run 34360434734)
// found moving between doctor runs: ReadEnforcementStats' HookCalls (grouped
// from hook_timing events, exactly what a leftover self-test record moves)
// and the events table's raw row and hook_timing-kind counts.
func countCallsAndEvents(t *testing.T) (statsHookCalls, totalEvents, timingEvents int) {
	t.Helper()
	rt, done := hookStoreRead(t)
	defer done()
	stats, err := rt.Store.ReadEnforcementStats(rt.Ctx, 24*time.Hour)
	if err != nil {
		t.Fatalf("read enforcement stats: %v", err)
	}
	evs, err := rt.Store.ListEvents(rt.Ctx)
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for i := range evs {
		if evs[i].Kind == store.EventHookTiming {
			timingEvents++
		}
	}
	return stats.HookCalls, len(evs), timingEvents
}

// TestDoctorEnforcement_SelfTestLeavesNoResidue is the PR #344 review fix
// (CI run 34360434734): the self-test's own synthetic call record and
// hook_timing events must not survive the leg that wrote them, or
// ReadEnforcementStats' counters (loto-ea8y.8, merged after this bead first
// shipped) move on every subsequent doctor run and the byte-identical-output
// AC breaks. One doctor run must be a no-op on both counters.
func TestDoctorEnforcement_SelfTestLeavesNoResidue(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	hookCallsBefore, eventsBefore, timingBefore := countCallsAndEvents(t)
	runOK(t, tcCmdDoctor)
	hookCallsAfter, eventsAfter, timingAfter := countCallsAndEvents(t)

	if hookCallsAfter != hookCallsBefore {
		t.Errorf("enforcement-stats hook_calls must be unchanged by one doctor run: before=%d after=%d", hookCallsBefore, hookCallsAfter)
	}
	if timingAfter != timingBefore {
		t.Errorf("hook_timing events must be unchanged by one doctor run: before=%d after=%d", timingBefore, timingAfter)
	}
	if eventsAfter != eventsBefore {
		t.Errorf("events table row count must be unchanged by one doctor run: before=%d after=%d", eventsBefore, eventsAfter)
	}
}

// --- rule 6: post_missing not kept current ----------------------------------

func TestDoctorEnforcement_StalePostMissing(t *testing.T) {
	withTempProject(t)
	agent := pinAgent(t)

	rt, done := hookStoreRead(t)
	if _, err := rt.Store.RecordCallPre(rt.Ctx, store.HookCall{
		CallID:      "stale-call-1",
		OwnerUUID:   domain.AgentUUID(agent.UUID),
		SessionUUID: rt.SessionUUID,
		ToolName:    "Bash",
		TPre:        time.Now().Add(-1 * time.Hour),
	}, nil); err != nil {
		t.Fatalf("seed stale call: %v", err)
	}
	done()

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ call_retention stale=1 first=stale-call-1") {
		t.Errorf("expected the call_retention red row naming the stale call: %q", out)
	}
	if !strings.Contains(out, "```bash\nloto doctor") {
		t.Errorf("expected a bash fix block under the ✗ row: %q", out)
	}
}

func TestDoctorEnforcement_NoStalePostMissing(t *testing.T) {
	withTempProject(t)
	pinAgent(t)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ call_retention no unmarked stale calls") {
		t.Errorf("expected the call_retention ✓ row: %q", out)
	}
}

// --- rule 7 / R8: shared-checkout pre-commit guard reachable ---------------

func TestDoctorEnforcement_SharedGuardGlobalUnset(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	unsetGlobalHooksPath(t)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ shared_guard reason=global-hooksPath-unset") {
		t.Errorf("expected the global-hooksPath-unset red row: %q", out)
	}
}

func TestDoctorEnforcement_SharedGuardFileAbsent(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	dir := writeSharedGuardFixture(t, true, false)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ shared_guard reason=unreachable detail="+filepath.Join(dir, "pre-commit")) {
		t.Errorf("expected the unreachable red row naming the target: %q", out)
	}
}

func TestDoctorEnforcement_SharedGuardMarkerAbsent(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	dir := writeSharedGuardFixture(t, false, true)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✗ shared_guard reason=marker-absent detail="+filepath.Join(dir, "pre-commit")) {
		t.Errorf("expected the marker-absent red row naming the target: %q", out)
	}
}

func TestDoctorEnforcement_SharedGuardReachable(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	dir := writeSharedGuardFixture(t, true, true)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ shared_guard reachable entry="+filepath.Join(dir, "pre-commit")) {
		t.Errorf("expected the shared_guard ✓ row: %q", out)
	}
}

// TestDoctorEnforcement_SharedGuardRelativeGlobalPath is PR #344 review
// (Copilot): a relative GLOBAL core.hooksPath must resolve against the repo
// top the same way resolveGitHooksPath already does for the effective value
// — not read as absent. A relative core.hooksPath is unusual but valid git
// config, and the prior implementation false-reasoned "" and mis-scored it.
func TestDoctorEnforcement_SharedGuardRelativeGlobalPath(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "gitconfig"))
	rel := "relhooks"
	// git rev-parse --show-toplevel resolves symlinks (macOS: /var is itself a
	// symlink to /private/var), so the repo top the running code resolves
	// this relative path against is EvalSymlinks(repo), not repo verbatim —
	// same class of mismatch the nested-worktree test guards against above.
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(resolvedRepo, rel)
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "#!/bin/sh\n# " + sharedGuardMarker + "\nexit 0\n"
	if err := os.WriteFile(filepath.Join(abs, "pre-commit"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	submitGitT(t, t.TempDir(), "config", "--global", "core.hooksPath", rel)

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "✓ shared_guard reachable entry="+filepath.Join(abs, "pre-commit")) {
		t.Errorf("expected a relative global core.hooksPath to resolve against the repo top: %q", out)
	}
}

// --- §11 advisory: nested worktree -----------------------------------------

func TestDoctorEnforcement_NestedWorktreeAdvisory(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	// Suffixes, not full paths: t.TempDir()'s own return and what git reports
	// back for a path underneath it can disagree on a macOS /var vs
	// /private/var symlink prefix, so an exact-path comparison is the wrong
	// tool here — the advisory's OWN row is what pins the full path.
	const innerSuffix = ".claude/worktrees/lane1/.claude/worktrees/lane2"
	const outerSuffix = ".claude/worktrees/lane1"
	outer := filepath.Join(repo, ".claude", "worktrees", "lane1")
	inner := filepath.Join(outer, ".claude", "worktrees", "lane2")
	submitGitT(t, repo, "worktree", "add", outer, "-b", "lane1")
	submitGitT(t, repo, "worktree", "add", inner, "-b", "lane2")

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "⚠ nested_worktree inner=") || !strings.Contains(out, innerSuffix) || !strings.Contains(out, outerSuffix) {
		t.Errorf("expected the nested_worktree advisory naming both paths: %q", out)
	}
	if !strings.Contains(out, "```bash\ngit worktree remove ") || !strings.Contains(out, innerSuffix+"'\n```") {
		t.Errorf("expected a bash fix block under the advisory: %q", out)
	}

	submitGitT(t, repo, "worktree", "remove", inner)
	cleared := runOK(t, tcCmdDoctor)
	if strings.Contains(cleared, "nested_worktree") {
		t.Errorf("expected the advisory to clear once the nested worktree is removed: %q", cleared)
	}
}

// --- composite: all seven, and byte-identical across runs ------------------

// TestDoctorEnforcement_AllSevenGreen is the bead's own AC: with none of the
// seven conditions induced, doctor prints ✓ for all seven, and output is
// byte-identical across two runs on the same state.
func TestDoctorEnforcement_AllSevenGreen(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeHookFixture(t, repo, "pre-commit", "post-checkout", "reference-transaction")
	setHooksPath(t, repo, ".githooks")
	writeTreeHookSettings(t)
	same := filepath.Join(repo, "loto-same")
	if err := os.WriteFile(same, []byte("one-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	sameFileSeams(t, same)
	writeSharedGuardFixture(t, true, true)

	first := runOK(t, tcCmdDoctor)
	for _, want := range []string{
		"✓ guard=pre-commit-gate reachable",
		"✓ guard=tree-move-guard reachable",
		"✓ guard=ref-transaction-guard reachable",
		"✓ tree_hooks registered events=3",
		"✓ hook_binary match path=" + same,
		"✓ hook_selftest pre+post recorded",
		"✓ call_retention no unmarked stale calls",
		"✓ shared_guard reachable",
	} {
		if !strings.Contains(first, want) {
			t.Errorf("expected %q in a fully-healthy run: %q", want, first)
		}
	}
	if strings.Contains(first, "✗") {
		t.Errorf("no red row should appear when every condition is healthy: %q", first)
	}

	second := runOK(t, tcCmdDoctor)
	if first != second {
		t.Errorf("doctor's enforcement rows must be byte-identical across runs on unchanged state:\n first  %q\n second %q", first, second)
	}
}
