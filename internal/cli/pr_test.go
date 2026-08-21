package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"loto/internal/gate"
)

// ‡ No t.Parallel in this file: every test swaps newPRPublisher, a mutable
// package var, and under -race a parallel test that never touches it still
// races with one that does (φ gate's promoteBeforePhase3Fn).

const (
	tcCmdPR    = "pr"
	tcPRBeadA  = "loto-brg.a"
	tcPRBeadB  = "loto-brg.b"
	tcPRFileA  = "bridged-a.txt"
	tcPRFileB  = "bridged-b.txt"
	tcPRBranch = gate.BranchPrefix
	tcPRMain   = "main"
)

// --- the network half, faked ----------------------------------------------

// fakePublisher records every call the CLI would have made to git push and
// gh. Nothing here reaches the network, which is the point of the prPublisher
// seam existing at all.
type fakePublisher struct {
	pushed  []string
	created []prCreate
	open    map[string]prRef
	next    int
}

func (f *fakePublisher) Push(_ context.Context, _, _, branch string) error {
	f.pushed = append(f.pushed, branch)
	return nil
}

func (f *fakePublisher) FindPR(_ context.Context, _, branch string) (prRef, bool, error) {
	ref, ok := f.open[branch]
	return ref, ok, nil
}

func (f *fakePublisher) CreatePR(_ context.Context, _ string, req prCreate) (prRef, error) {
	f.created = append(f.created, req)
	f.next++
	ref := prRef{Number: f.next, URL: "https://github.com/test/proj/pull/" + strconv.Itoa(f.next)}
	if f.open == nil {
		f.open = map[string]prRef{}
	}
	f.open[req.Branch] = ref
	return ref, nil
}

func usePublisher(t *testing.T) *fakePublisher {
	t.Helper()
	f := &fakePublisher{open: map[string]prRef{}}
	prev := newPRPublisher
	newPRPublisher = func() prPublisher { return f }
	t.Cleanup(func() { newPRPublisher = prev })
	return f
}

// --- fixture --------------------------------------------------------------

func prGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// prBaseRepo commits the withTempProject fixture on main, then parks HEAD on
// an integration branch refs/loto/integration follows — the shape prPlant
// appends promoted commits to.
func prBaseRepo(t *testing.T) string {
	t.Helper()
	repo := withTempProject(t)
	prGitT(t, repo, "add", "-A")
	prGitT(t, repo, "commit", "-q", "-m", "base")
	prGitT(t, repo, "branch", "-M", tcPRMain)
	prGitT(t, repo, "checkout", "-q", "-b", "integ")
	prGitT(t, repo, "update-ref", "refs/loto/integration", "HEAD")
	return repo
}

// prPlant appends one promotion-shaped commit — the trailers gate.PlanBridge
// groups on — and advances refs/loto/integration onto it.
func prPlant(t *testing.T, repo, bead, cand, file, content string) string {
	t.Helper()
	writeTestFile(t, repo, file, content)
	prGitT(t, repo, "add", "--", file)
	msg := "loto: promote " + cand + "\n\nCandidate: " + cand + "\nAgent: agent-a\nSession: agent-a-s\n"
	if bead != "" {
		msg += "Bead: " + bead + "\n"
	}
	prGitT(t, repo, "commit", "-q", "-m", msg)
	sha := prGitT(t, repo, "rev-parse", "HEAD")
	prGitT(t, repo, "update-ref", "refs/loto/integration", sha)
	return sha
}

func writeTestFile(t *testing.T, repo, rel, content string) {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func refMissing(t *testing.T, repo, ref string) bool {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--verify", "--quiet", ref)
	cmd.Dir = repo
	return cmd.Run() != nil
}

// --- acceptance -----------------------------------------------------------

// TestPR_IntegrationAbsentIsNeutral: nothing promoted yet is not a failure,
// and the header must say so out loud — silence reads as a crash (design.md).
func TestPR_IntegrationAbsentIsNeutral(t *testing.T) {
	usePublisher(t)
	repo := withTempProject(t)
	prGitT(t, repo, "add", "-A")
	prGitT(t, repo, "commit", "-q", "-m", "base")
	prGitT(t, repo, "branch", "-M", tcPRMain)

	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "integration=absent") || !strings.HasPrefix(stdout, "ℹ pr beads=0") {
		t.Errorf("empty-status header wrong:\n%s", stdout)
	}
	if !refMissing(t, repo, "refs/loto/integration") {
		t.Error("loto pr bootstrapped refs/loto/integration")
	}
}

// TestPR_OpensOnePRPerBead is the bead's contract: two beads interleaved on
// integration become two branches and two non-draft PRs, each carrying its
// own Closes: trailer.
func TestPR_OpensOnePRPerBead(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")
	prPlant(t, repo, tcPRBeadB, "cand-b1", tcPRFileB, "B1\n")
	prPlant(t, repo, tcPRBeadA, "cand-a2", tcPRFileA, "A2\n")

	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "✓ pr beads=2 opened=2 updated=0 current=0 blocked=0 unattributed=0\n") {
		t.Errorf("triage line wrong:\n%s", stdout)
	}
	for _, want := range []string{
		"✓ bead=" + tcPRBeadA + " branch=loto-pr/" + tcPRBeadA + " commits=2 pr=",
		"✓ bead=" + tcPRBeadB + " branch=loto-pr/" + tcPRBeadB + " commits=1 pr=",
		"action=opened",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}

	if len(f.created) != 2 {
		t.Fatalf("created %d PRs, want 2: %+v", len(f.created), f.created)
	}
	for _, req := range f.created {
		if req.Base != tcPRMain {
			t.Errorf("PR base = %q, want main", req.Base)
		}
		if !strings.HasPrefix(req.Branch, tcPRBranch) {
			t.Errorf("PR head = %q, want a loto-pr/ branch", req.Branch)
		}
		bead := strings.TrimPrefix(req.Branch, tcPRBranch)
		if !strings.Contains(req.Body, "Closes: "+bead) {
			t.Errorf("PR body for %s lacks its Closes: trailer:\n%s", bead, req.Body)
		}
		if !strings.HasPrefix(req.Title, bead+": ") {
			t.Errorf("PR title = %q, want it to lead with the bead id", req.Title)
		}
	}
	// Every push is a bead branch. main is never a push target and never a
	// merge target — the PR is the human gate.
	if len(f.pushed) != 2 {
		t.Fatalf("pushed %v, want the two bead branches", f.pushed)
	}
	for _, b := range f.pushed {
		if !strings.HasPrefix(b, tcPRBranch) {
			t.Errorf("pushed %q, which is not a bead branch", b)
		}
	}
}

// TestPR_SecondRunOpensNothing: the whole verb converges. Re-running must
// push nothing new, open no duplicate PR, and report the beads as current.
func TestPR_SecondRunOpensNothing(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")

	if _, stderr, code := executeCommand(tcCmdPR); code != 0 {
		t.Fatalf("first run: exit %d\n%s", code, stderr)
	}
	head := prGitT(t, repo, "rev-parse", "refs/heads/loto-pr/"+tcPRBeadA)

	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 0 {
		t.Fatalf("second run: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "✓ pr beads=1 opened=0 updated=0 current=1 blocked=0 unattributed=0\n") {
		t.Errorf("second-run triage line wrong:\n%s", stdout)
	}
	if !strings.Contains(stdout, "action="+prActionAlreadyOpen) {
		t.Errorf("second run did not report the PR as already open:\n%s", stdout)
	}
	if len(f.created) != 1 {
		t.Errorf("created %d PRs across two runs, want 1", len(f.created))
	}
	if got := prGitT(t, repo, "rev-parse", "refs/heads/loto-pr/"+tcPRBeadA); got != head {
		t.Errorf("the bead branch moved on a second run: %s -> %s", head, got)
	}
}

// TestPR_NewWorkUpdatesTheOpenPR: a bead that promotes again while its PR is
// open gets the commit appended and no second PR.
func TestPR_NewWorkUpdatesTheOpenPR(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")
	executeCommand(tcCmdPR)

	prPlant(t, repo, tcPRBeadA, "cand-a2", tcPRFileA, "A2\n")
	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "action="+prActionUpdated) {
		t.Errorf("want an updated row:\n%s", stdout)
	}
	if len(f.created) != 1 {
		t.Errorf("created %d PRs, want the original one only", len(f.created))
	}
	if got := prGitT(t, repo, "rev-list", "--count", "main..refs/heads/loto-pr/"+tcPRBeadA); got != "2" {
		t.Errorf("branch carries %s commits, want 2", got)
	}
}

// TestPR_DryRunTouchesNothing: --dry-run must write no ref and reach no
// remote, so it is safe to run inside a shared checkout at any time. Also
// pins determinism (design.md) — two runs over one state, byte-identical —
// because that assertion needs the same fixture and the fixture is the cost.
func TestPR_DryRunTouchesNothing(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadB, "cand-b1", tcPRFileB, "B1\n")
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")

	stdout, stderr, code := executeCommand(tcCmdPR, "--dry-run")
	if code != 0 {
		t.Fatalf("exit = %d\n%s%s", code, stdout, stderr)
	}
	// opened/updated stay at zero — a dry run opened and updated nothing —
	// and what it would carry gets its own key.
	if !strings.HasPrefix(stdout, "✓ pr beads=2 opened=0 updated=0 current=0 blocked=0 unattributed=0 pending=2 dry-run=true\n") {
		t.Errorf("dry-run triage line wrong:\n%s", stdout)
	}
	if !strings.Contains(stdout, "action="+prActionWouldBuild) {
		t.Errorf("dry-run report wrong:\n%s", stdout)
	}
	if !refMissing(t, repo, "refs/heads/loto-pr/"+tcPRBeadA) {
		t.Error("--dry-run wrote the bead branch")
	}
	if !refMissing(t, repo, "refs/loto/bridged/"+tcPRBeadA) {
		t.Error("--dry-run wrote the bridged marker")
	}
	if len(f.pushed) != 0 || len(f.created) != 0 {
		t.Errorf("--dry-run reached the remote: pushed=%v created=%d", f.pushed, len(f.created))
	}

	second, _, _ := executeCommand(tcCmdPR, "--dry-run")
	if second != stdout {
		t.Errorf("two runs over the same state differ:\n%s\n---\n%s", stdout, second)
	}
	// Rows sort by line, so bead A precedes bead B whatever order they
	// promoted in — and B promoted first above.
	if strings.Index(stdout, tcPRBeadA) > strings.Index(stdout, tcPRBeadB) {
		t.Errorf("rows are not sorted deterministically:\n%s", stdout)
	}
}

// TestPR_StaleBaseBlocksAndTeaches: main moved under a path the bead
// promoted. Replaying would revert it, so the run refuses, exits 1, names the
// path, and prints the command that resolves it.
func TestPR_StaleBaseBlocksAndTeaches(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")
	prGitT(t, repo, "checkout", "-q", tcPRMain)
	writeTestFile(t, repo, tcPRFileA, "someone else got here first\n")
	prGitT(t, repo, "add", "--", tcPRFileA)
	prGitT(t, repo, "commit", "-q", "-m", "main moves")
	prGitT(t, repo, "checkout", "-q", "integ")

	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n%s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, "✗ pr beads=1 opened=0 updated=0 current=0 blocked=1 unattributed=0\n") {
		t.Errorf("triage line wrong:\n%s", stdout)
	}
	for _, want := range []string{"reason=stale-base", "target=" + tcPRFileA, "```bash"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in:\n%s", want, stdout)
		}
	}
	if len(f.created) != 0 {
		t.Error("a refused bead still got a PR")
	}
}

// TestPR_UnattributedCommitIsAdvisory: a promoted commit with no Bead:
// trailer cannot be given a Closes:, so it is surfaced as a ⚠ row and holds
// up nobody's PR.
func TestPR_UnattributedCommitIsAdvisory(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, "", "cand-x", tcPRFileB, "orphan\n")
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")

	stdout, stderr, code := executeCommand(tcCmdPR)
	if code != 0 {
		t.Fatalf("exit = %d, want 0\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unattributed=1") {
		t.Errorf("triage line lost the unattributed count:\n%s", stdout)
	}
	if !strings.Contains(stdout, "⚠ commit=") || !strings.Contains(stdout, "reason=no-bead-trailer candidate=cand-x") {
		t.Errorf("missing the advisory row:\n%s", stdout)
	}
	if len(f.created) != 1 {
		t.Errorf("created %d PRs, want only the attributed bead's", len(f.created))
	}
}

// TestPR_BeadFilter narrows a run to one bead.
func TestPR_BeadFilter(t *testing.T) {
	f := usePublisher(t)
	repo := prBaseRepo(t)
	prPlant(t, repo, tcPRBeadA, "cand-a1", tcPRFileA, "A1\n")
	prPlant(t, repo, tcPRBeadB, "cand-b1", tcPRFileB, "B1\n")

	if _, stderr, code := executeCommand(tcCmdPR, "--bead", tcPRBeadB); code != 0 {
		t.Fatalf("exit %d\n%s", code, stderr)
	}
	if len(f.created) != 1 || f.created[0].Branch != tcPRBranch+tcPRBeadB {
		t.Errorf("created %+v, want only bead B's PR", f.created)
	}
	if !refMissing(t, repo, "refs/heads/loto-pr/"+tcPRBeadA) {
		t.Error("--bead built a branch for the bead it was told to skip")
	}
}

// TestPR_RefusesToPushAnythingButABeadBranch pins the one hard stop: this
// verb must never be what writes main. Reached directly, because nothing
// upstream can produce a non-bead branch — which is the point of the guard.
func TestPR_RefusesToPushAnythingButABeadBranch(t *testing.T) {
	f := usePublisher(t)

	// t.TempDir, not prBaseRepo: the guard fires before any git call, so
	// building a repo would only buy wall-clock.
	_, err := bridgeBead(context.Background(), t.TempDir(),
		prOpts{base: tcPRMain, remote: "origin"}, f,
		&gate.BeadBridge{BeadID: tcPRBeadA, Branch: "main", Class: gate.BridgeUpToDate})
	if !errors.Is(err, errPRNotABeadBranch) {
		t.Errorf("err = %v, want errPRNotABeadBranch", err)
	}
	if len(f.pushed) != 0 {
		t.Errorf("pushed %v despite the guard", f.pushed)
	}
}

// TestPR_RejectsPositionalArgs keeps the flag surface honest.
func TestPR_RejectsPositionalArgs(t *testing.T) {
	usePublisher(t)
	if _, _, code := executeCommand(tcCmdPR, "stray"); code != 2 {
		t.Errorf("exit = %d, want 2", code)
	}
}

// TestPRTitle_TruncatesAWideWriteSet keeps a PR list scannable.
func TestPRTitle_TruncatesAWideWriteSet(t *testing.T) {
	got := prTitle(&gate.BeadBridge{
		BeadID: tcPRBeadA,
		WriteSet: []string{
			"internal/cli/cmd_status.go", "internal/cli/cmd_sync.go",
			"internal/gate/bridge.go", "internal/gate/promotion.go",
		},
	})
	if len(got) > prTitleMax {
		t.Errorf("title is %d chars, want <= %d: %q", len(got), prTitleMax, got)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("a truncated title should say so: %q", got)
	}
}
