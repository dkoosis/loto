package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// stashGitT runs git in dir and fails the test on error, mirroring the
// per-file `<prefix>GitT` helper convention (syncGitT, submitGitT, prGitT).
func stashGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// seedStashableCommit gives repo an initial commit: `git stash push` refuses
// on a repo with no commits at all, and withTempProject only writes files to
// the working tree without committing them.
func seedStashableCommit(t *testing.T, repo string) {
	t.Helper()
	stashGitT(t, repo, "add", "-A")
	stashGitT(t, repo, "commit", "-q", "-m", "seed")
}

// pushStash edits tcTargetA and stashes the change with the given message,
// returning nothing — callers read the row back through `loto doctor`.
func pushStash(t *testing.T, repo, message string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stashGitT(t, repo, "stash", "push", "-u", "-m", message)
}

// ageFieldRe strips a stash row's wall-clock-dependent age= value so two
// reads taken moments apart can still be compared byte-for-byte — the same
// normalization a real clock forces on any "age since creation" field
// (EmitCandidateClaimConflict's age= is the same shape, store/render side).
var ageFieldRe = regexp.MustCompile(`age=\S+`)

func normalizeAge(s string) string { return ageFieldRe.ReplaceAllString(s, "age=<age>") }

func TestDoctorReportsDanglingStashWithDeadAgent(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	seedStashableCommit(t, repo)

	deadAgent := "dead-agent-uuid-0001"
	pushStash(t, repo, "rescue LOTO_AGENT_ID="+deadAgent+" mid-edit")

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "ℹ dangling_stashes count=1") {
		t.Fatalf("expected one dangling stash row: %q", out)
	}
	if !strings.Contains(out, "ref=stash@{0}") {
		t.Errorf("row must name the stash ref: %q", out)
	}
	if !strings.Contains(out, "agent="+deadAgent) {
		t.Errorf("row must attribute the dead agent: %q", out)
	}
	if !strings.Contains(out, "age=") {
		t.Errorf("row must carry an age: %q", out)
	}
	if !strings.Contains(out, "paths="+tcTargetA) {
		t.Errorf("row must name the touched path: %q", out)
	}
}

func TestDoctorStashNoStampPrintsAgentUnknown(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	seedStashableCommit(t, repo)

	pushStash(t, repo, "unattributed rescue stash")

	out := runOK(t, tcCmdDoctor)
	if !strings.Contains(out, "agent=unknown") {
		t.Fatalf("unstamped stash must print agent=unknown: %q", out)
	}
}

// TestDoctorStashLiveAgentSuppressed is Rule 1's other half: a stash stamped
// with an agent that currently holds a lock is live, not dangling, and must
// not print a row at all.
func TestDoctorStashLiveAgentSuppressed(t *testing.T) {
	repo := withTempProject(t)
	live := pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, tcTargetB), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	runOK(t, tcCmdLock, tcTargetB, "-t", tcIntentTest)
	seedStashableCommit(t, repo)

	pushStash(t, repo, "rescue LOTO_AGENT_ID="+live.UUID+" mid-edit")

	out := runOK(t, tcCmdDoctor)
	if strings.Contains(out, "dangling_stashes") {
		t.Errorf("a live agent's stash must not be reported: %q", out)
	}
}

// TestDoctorRepairNeverTouchesStash is loto-kotb's central invariant (D8):
// `--repair` reclaims stale locks and claims, but a stash is bytes, not a
// lock row, and doctor never pops/applies/drops one.
func TestDoctorRepairNeverTouchesStash(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	seedStashableCommit(t, repo)
	pushStash(t, repo, "rescue LOTO_AGENT_ID=dead-agent-uuid-0002 mid-edit")

	before := stashGitT(t, repo, "stash", "list")
	runOK(t, tcCmdDoctor, tcFlagRepair)
	after := stashGitT(t, repo, "stash", "list")

	if before != after {
		t.Fatalf("--repair must leave `git stash list` unchanged:\n before %q\n after  %q", before, after)
	}
}

// TestDoctorStashRowByteIdenticalAcrossRuns is the golden-test requirement:
// same repo state, two reads a moment apart, same row modulo the wall-clock
// age field (normalized before comparing — see ageFieldRe).
func TestDoctorStashRowByteIdenticalAcrossRuns(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	seedStashableCommit(t, repo)
	pushStash(t, repo, "rescue LOTO_AGENT_ID=dead-agent-uuid-0003 mid-edit")

	first := normalizeAge(runOK(t, tcCmdDoctor))
	second := normalizeAge(runOK(t, tcCmdDoctor))
	if first != second {
		t.Errorf("doctor's stash row must be stable across runs on unchanged state:\n first  %q\n second %q", first, second)
	}
}
