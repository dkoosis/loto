//go:build unix

package gate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ‡ No t.Parallel anywhere in package gate — promotion.go's
// promoteBeforePhase3Fn is a mutable package var, and under -race a parallel
// test that never touches it still races with one that does.

// Fixture literals, hoisted per goconst — every scenario below reuses the same
// handful of beads, files and contents in a fresh repo.
const (
	tbBeadA  = "loto-brg.a"
	tbBeadB  = "loto-brg.b"
	tbInteg  = "integ"
	tbMain   = "main"
	tbFileC  = "c.go"
	tbCandA1 = "cand-a1"
	tbCandA2 = "cand-a2"
	tbCandB1 = "cand-b1"
	tbA1     = "A1\n"
	tbA2     = "A2\n"
	tbB1     = "B1\n"
)

// --- fixture -------------------------------------------------------------

// newBridgeRepo returns a repo whose refs/loto/integration tracks a branch
// ahead of main, ready for plantPromoted to append to.
//
// ‡ The fixture checks out a branch, which the production path never does.
// That is deliberate: the bridge's job is to read integration history and
// replay it by plumbing, and building the history the ordinary way keeps the
// trailers legible to whoever reads this test next. TestBridge_EndToEnd is
// the one that proves the shape against real promotion output.
func newBridgeRepo(t *testing.T) string {
	t.Helper()
	repoTop, _ := newIntegrationRepo(t)
	gitT(t, repoTop, "checkout", "-q", "-b", tbInteg)
	gitT(t, repoTop, "update-ref", IntegrationRef, "HEAD")
	return repoTop
}

// promotedMsg is the message shape promotion's commitTree writes.
func promotedMsg(cand, bead string) string {
	return "loto: promote " + cand + "\n\n" +
		"Candidate: " + cand + "\n" +
		"Proposal: " + strings.Repeat("0", 40) + "\n" +
		"Agent: agent-a\nSession: agent-a-s\n" +
		"Bead: " + bead + "\n"
}

// plantPromoted appends one promotion-shaped commit to the integration branch
// and advances refs/loto/integration to it. An empty file value deletes.
func plantPromoted(t *testing.T, repoTop, bead, cand string, files map[string]string) string {
	t.Helper()
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if files[p] == "" {
			gitT(t, repoTop, "rm", "-q", "--", p)
			continue
		}
		writeFile(t, repoTop, p, files[p])
		gitT(t, repoTop, "add", "--", p)
	}
	gitT(t, repoTop, "commit", "-q", "-m", promotedMsg(cand, bead))
	sha := gitT(t, repoTop, "rev-parse", "HEAD")
	gitT(t, repoTop, "update-ref", IntegrationRef, sha)
	return sha
}

// advanceMain commits on main and returns to the integration branch — how a
// merged PR (or a hand edit) moves the fourth authority level under us.
func advanceMain(t *testing.T, repoTop, path, content string) {
	t.Helper()
	gitT(t, repoTop, "checkout", "-q", tbMain)
	writeFile(t, repoTop, path, content)
	gitT(t, repoTop, "add", "--", path)
	gitT(t, repoTop, "commit", "-q", "-m", "main moves under the bridge")
	gitT(t, repoTop, "checkout", "-q", tbInteg)
}

func mustPlan(t *testing.T, repoTop string) BridgePlan {
	t.Helper()
	plan, err := PlanBridge(context.Background(), BridgeParams{RepoTop: repoTop})
	if err != nil {
		t.Fatalf("PlanBridge: %v", err)
	}
	return plan
}

func beadIn(t *testing.T, plan BridgePlan, bead string) *BeadBridge {
	t.Helper()
	for i := range plan.Beads {
		if plan.Beads[i].BeadID == bead {
			return &plan.Beads[i]
		}
	}
	t.Fatalf("no plan entry for %s (have %d beads)", bead, len(plan.Beads))
	return nil
}

func mustBuild(t *testing.T, repoTop string, b *BeadBridge) string {
	t.Helper()
	if err := BuildBridge(context.Background(), BridgeParams{RepoTop: repoTop}, b); err != nil {
		t.Fatalf("BuildBridge %s: %v", b.BeadID, err)
	}
	return b.Head
}

// treeHas reports the blob content at path in treeish (trailing whitespace
// trimmed by gitT), or "" when absent.
func treeHas(t *testing.T, repoTop, treeish, path string) string {
	t.Helper()
	if gitT(t, repoTop, "ls-tree", treeish, "--", path) == "" {
		return ""
	}
	return gitT(t, repoTop, "show", treeish+":"+path)
}

func countCommits(t *testing.T, repoTop, rng string) int {
	t.Helper()
	out := gitT(t, repoTop, "rev-list", "--count", rng)
	n := 0
	for _, r := range out {
		n = n*10 + int(r-'0')
	}
	return n
}

// --- acceptance 1: grouping + independent replay --------------------------

// TestPlanBridge_InterleavedBeadsBecomeIndependentBranches is the bead's
// central claim: integration interleaves two beads' commits, and each bead
// still crosses to main as its own branch carrying only its own paths.
func TestPlanBridge_InterleavedBeadsBecomeIndependentBranches(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	plantPromoted(t, repoTop, tbBeadB, tbCandB1, map[string]string{tfFileB: tbB1})
	plantPromoted(t, repoTop, tbBeadA, tbCandA2, map[string]string{tfFileA: tbA2})

	plan := mustPlan(t, repoTop)
	if len(plan.Beads) != 2 {
		t.Fatalf("beads = %d, want 2: %+v", len(plan.Beads), plan.Beads)
	}
	if plan.Beads[0].BeadID != tbBeadA || plan.Beads[1].BeadID != tbBeadB {
		t.Errorf("beads not sorted by id: %q, %q", plan.Beads[0].BeadID, plan.Beads[1].BeadID)
	}

	a, b := beadIn(t, plan, tbBeadA), beadIn(t, plan, tbBeadB)
	if a.Class != BridgeBuildable || b.Class != BridgeBuildable {
		t.Fatalf("classes = %q/%q, want both buildable", a.Class, b.Class)
	}
	if len(a.Pending) != 2 || len(b.Pending) != 1 {
		t.Fatalf("pending = %d/%d, want 2/1", len(a.Pending), len(b.Pending))
	}
	// Integration order within a bead: cand-a1 before cand-a2.
	if a.Pending[0].CandidateID != tbCandA1 || a.Pending[1].CandidateID != tbCandA2 {
		t.Errorf("bead A pending out of integration order: %q, %q", a.Pending[0].CandidateID, a.Pending[1].CandidateID)
	}
	if len(a.WriteSet) != 1 || a.WriteSet[0] != tfFileA {
		t.Errorf("bead A write-set = %v, want [%s]", a.WriteSet, tfFileA)
	}
	// Both branches fork from main, not from each other.
	if a.Parent != plan.MainSHA || b.Parent != plan.MainSHA {
		t.Errorf("parents = %q/%q, want main %q", a.Parent, b.Parent, plan.MainSHA)
	}

	headA, headB := mustBuild(t, repoTop, a), mustBuild(t, repoTop, b)
	if got := countCommits(t, repoTop, tbMain+".."+headA); got != 2 {
		t.Errorf("branch A carries %d commits, want 2", got)
	}
	if got := countCommits(t, repoTop, tbMain+".."+headB); got != 1 {
		t.Errorf("branch B carries %d commits, want 1", got)
	}
	// The disjointness claim, checked: neither branch carries the other's file.
	if got := treeHas(t, repoTop, headA, tfFileA); got != strings.TrimSpace(tbA2) {
		t.Errorf("branch A: %s = %q, want the bead's last content", tfFileA, got)
	}
	if got := treeHas(t, repoTop, headA, tfFileB); got != "" {
		t.Errorf("branch A leaked bead B's %s: %q", tfFileB, got)
	}
	if got := treeHas(t, repoTop, headB, tfFileB); got != strings.TrimSpace(tbB1) {
		t.Errorf("branch B: %s = %q", tfFileB, got)
	}
	if got := treeHas(t, repoTop, headB, tfFileA); got != "package gate\n\nvar A = 1" {
		t.Errorf("branch B: %s = %q, want main's content untouched", tfFileA, got)
	}
	// The ref names the CLI pushes must exist under the branch namespace.
	if gitT(t, repoTop, "rev-parse", "--verify", a.Ref) != headA {
		t.Errorf("%s does not point at the built head", a.Ref)
	}
	if a.Branch != "loto-pr/"+tbBeadA {
		t.Errorf("branch name = %q", a.Branch)
	}
}

// --- acceptance 2: idempotence -------------------------------------------

// TestBuildBridge_SecondRunIsANoOp: running the bridge twice must not rebuild
// or duplicate anything. The bridged marker, written in the same transaction
// as the branch, is what makes the second plan see nothing to do.
func TestBuildBridge_SecondRunIsANoOp(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})

	first := mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))

	again := beadIn(t, mustPlan(t, repoTop), tbBeadA)
	if again.Class != BridgeUpToDate {
		t.Errorf("second plan class = %q, want up-to-date", again.Class)
	}
	if len(again.Pending) != 0 {
		t.Errorf("second plan has %d pending commits, want 0", len(again.Pending))
	}
	if again.Head != first {
		t.Errorf("second plan head = %q, want the branch tip %q", again.Head, first)
	}
	// Building anyway is a caller bug, not a silent second write.
	err := BuildBridge(context.Background(), BridgeParams{RepoTop: repoTop}, again)
	if !errors.Is(err, errBridgeNotBuildable) {
		t.Errorf("BuildBridge on an up-to-date bead: err = %v, want errBridgeNotBuildable", err)
	}
	if gitT(t, repoTop, "rev-parse", "--verify", again.Ref) != first {
		t.Error("the branch moved on a second run")
	}
}

// TestBuildBridge_RebuildIsByteIdentical: with the branch and marker cleared,
// the same integration state must rebuild to the SAME commit SHA. That is
// what makes a crash mid-run safe to re-enter — the rebuild converges on the
// history the interrupted run was writing rather than forking a new one.
func TestBuildBridge_RebuildIsByteIdentical(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	plantPromoted(t, repoTop, tbBeadA, tbCandA2, map[string]string{tfFileA: tbA2})

	b := beadIn(t, mustPlan(t, repoTop), tbBeadA)
	first := mustBuild(t, repoTop, b)

	gitT(t, repoTop, "update-ref", "-d", b.Ref)
	gitT(t, repoTop, "update-ref", "-d", bridgedRefPrefix+tbBeadA)

	second := mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))
	if first != second {
		t.Errorf("rebuild produced %s, want the identical %s", second, first)
	}
}

// TestBuildBridge_AppendsNewWorkToTheOpenBranch: a bead that promotes again
// while its PR is open gets the new commit appended — never a rebuilt branch,
// which would rewrite history under a review in progress.
func TestBuildBridge_AppendsNewWorkToTheOpenBranch(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	first := mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))

	plantPromoted(t, repoTop, tbBeadA, tbCandA2, map[string]string{tfFileA: tbA2})
	next := beadIn(t, mustPlan(t, repoTop), tbBeadA)
	if next.Class != BridgeBuildable {
		t.Fatalf("class = %q, want buildable", next.Class)
	}
	if next.Parent != first {
		t.Errorf("parent = %q, want the existing branch tip %q", next.Parent, first)
	}
	if len(next.Pending) != 1 || next.Pending[0].CandidateID != tbCandA2 {
		t.Fatalf("pending = %+v, want only cand-a2", next.Pending)
	}
	second := mustBuild(t, repoTop, next)
	if got := countCommits(t, repoTop, first+".."+second); got != 1 {
		t.Errorf("branch grew by %d commits, want 1", got)
	}
	if !isAncestor(t, repoTop, first, second) {
		t.Error("the branch was rewritten, not fast-forwarded")
	}
}

func isAncestor(t *testing.T, repoTop, a, b string) bool {
	t.Helper()
	return gitT(t, repoTop, "merge-base", a, b) == a
}

// --- acceptance 3: refusals ----------------------------------------------

// TestPlanBridge_StaleBaseRefusesAReplayThatWouldRevertMain: main moved under
// a path the bead promoted. Replaying would set it back to the blob
// integration knew about, silently reverting whatever landed. Refused, and
// the offending path is named.
func TestPlanBridge_StaleBaseRefusesAReplayThatWouldRevertMain(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	advanceMain(t, repoTop, tfFileA, "someone else got here first\n")

	b := beadIn(t, mustPlan(t, repoTop), tbBeadA)
	if b.Class != BridgeStaleBase {
		t.Fatalf("class = %q, want stale-base", b.Class)
	}
	if b.Detail != tfFileA {
		t.Errorf("detail = %q, want the offending path %q", b.Detail, tfFileA)
	}
	if refExists(t, repoTop, b.Ref) {
		t.Error("a refused bead still got a branch")
	}
}

// TestPlanBridge_StaleBaseIgnoresPathsTheBeadNeverTouched: main moving under
// an UNRELATED path must not block a bead. That is the lease-disjointness
// assumption doing its work — the check is per-path, not per-tree.
func TestPlanBridge_StaleBaseIgnoresPathsTheBeadNeverTouched(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileB: tbB1})
	advanceMain(t, repoTop, tfFileA, "unrelated churn\n")

	if got := beadIn(t, mustPlan(t, repoTop), tbBeadA).Class; got != BridgeBuildable {
		t.Errorf("class = %q, want buildable", got)
	}
}

// TestPlanBridge_UnattributedCommitIsReportedNeverBridged: a promoted commit
// with no Bead: trailer cannot be grouped or given a Closes:, so it is
// surfaced and left alone rather than folded into someone else's PR.
func TestPlanBridge_UnattributedCommitIsReportedNeverBridged(t *testing.T) {
	repoTop := newBridgeRepo(t)
	writeFile(t, repoTop, tbFileC, "orphan\n")
	gitT(t, repoTop, "add", "--", tbFileC)
	gitT(t, repoTop, "commit", "-q", "-m", "loto: promote cand-x\n\nCandidate: cand-x\n")
	gitT(t, repoTop, "update-ref", IntegrationRef, "HEAD")

	plan := mustPlan(t, repoTop)
	if len(plan.Beads) != 0 {
		t.Errorf("beads = %+v, want none", plan.Beads)
	}
	if len(plan.Unattributed) != 1 || plan.Unattributed[0].CandidateID != "cand-x" {
		t.Fatalf("unattributed = %+v, want the one orphan commit", plan.Unattributed)
	}
}

// TestPlanBridge_StaleBranchShapes pins each way the branch ref and the
// bridged marker can disagree. Every one of them is a human having moved a
// ref; the bridge reports and refuses rather than guessing.
func TestPlanBridge_StaleBranchShapes(t *testing.T) {
	t.Run("branch-missing", func(t *testing.T) {
		repoTop := newBridgeRepo(t)
		plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
		b := beadIn(t, mustPlan(t, repoTop), tbBeadA)
		mustBuild(t, repoTop, b)
		gitT(t, repoTop, "update-ref", "-d", b.Ref)

		got := beadIn(t, mustPlan(t, repoTop), tbBeadA)
		if got.Class != BridgeStaleBranch || got.Detail != detailBranchMissing {
			t.Errorf("class/detail = %q/%q, want stale-branch/%s", got.Class, got.Detail, detailBranchMissing)
		}
	})

	t.Run("unmarked-branch", func(t *testing.T) {
		repoTop := newBridgeRepo(t)
		plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
		gitT(t, repoTop, "update-ref", bridgeBranchPrefix+tbBeadA, tbMain)

		got := beadIn(t, mustPlan(t, repoTop), tbBeadA)
		if got.Class != BridgeStaleBranch || got.Detail != detailUnmarked {
			t.Errorf("class/detail = %q/%q, want stale-branch/%s", got.Class, got.Detail, detailUnmarked)
		}
	})

	t.Run("merged-branch", func(t *testing.T) {
		repoTop := newBridgeRepo(t)
		landed := plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
		mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))
		// The PR merged: main now contains the bridged commit, so it drops out
		// of main..integration and the marker no longer names a listed commit.
		gitT(t, repoTop, "update-ref", "refs/heads/"+tbMain, landed)
		plantPromoted(t, repoTop, tbBeadA, tbCandA2, map[string]string{tfFileA: tbA2})

		got := beadIn(t, mustPlan(t, repoTop), tbBeadA)
		if got.Class != BridgeStaleBranch || got.Detail != detailMergedBranch {
			t.Errorf("class/detail = %q/%q, want stale-branch/%s", got.Class, got.Detail, detailMergedBranch)
		}
	})

	t.Run("merged-and-branch-deleted-rebuilds-from-main", func(t *testing.T) {
		repoTop := newBridgeRepo(t)
		landed := plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
		b := beadIn(t, mustPlan(t, repoTop), tbBeadA)
		mustBuild(t, repoTop, b)
		gitT(t, repoTop, "update-ref", "refs/heads/"+tbMain, landed)
		gitT(t, repoTop, "update-ref", "-d", b.Ref) // GitHub's delete-branch-on-merge
		plantPromoted(t, repoTop, tbBeadA, tbCandA2, map[string]string{tfFileA: tbA2})

		plan := mustPlan(t, repoTop)
		got := beadIn(t, plan, tbBeadA)
		if got.Class != BridgeBuildable {
			t.Fatalf("class = %q, want buildable", got.Class)
		}
		if got.Parent != plan.MainSHA {
			t.Errorf("parent = %q, want main %q", got.Parent, plan.MainSHA)
		}
		if len(got.Pending) != 1 || got.Pending[0].CandidateID != tbCandA2 {
			t.Errorf("pending = %+v, want only the new commit", got.Pending)
		}
	})
}

// --- acceptance 4: transitions -------------------------------------------

// TestBuildBridge_DeletionRoundTrips: a promoted deletion must arrive on the
// branch as a deletion, not as a path the replay quietly kept.
func TestBuildBridge_DeletionRoundTrips(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: ""})

	b := beadIn(t, mustPlan(t, repoTop), tbBeadA)
	if len(b.Pending) != 1 || len(b.Pending[0].Transitions) != 1 || b.Pending[0].Transitions[0].Result != nil {
		t.Fatalf("transitions = %+v, want one deletion", b.Pending)
	}
	head := mustBuild(t, repoTop, b)
	if got := treeHas(t, repoTop, head, tfFileA); got != "" {
		t.Errorf("%s survived the replayed deletion: %q", tfFileA, got)
	}
}

// TestBuildBridge_CreatedPathCarriesItsDirectory: a created nested path must
// arrive with its parent trees, and must not disturb anything else.
func TestBuildBridge_CreatedPathCarriesItsDirectory(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{"pkg/sub/new.go": "made\n"})

	head := mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))
	if got := treeHas(t, repoTop, head, "pkg/sub/new.go"); got != "made" {
		t.Errorf("created path = %q", got)
	}
	if got := treeHas(t, repoTop, head, tfFileA); got == "" {
		t.Error("the replay dropped a file the bead never touched")
	}
}

// TestBuildBridge_PreservesAttributionAndNamesItsSource: the PR reviewer must
// be able to trace a branch commit back to the integration commit it replays.
func TestBuildBridge_PreservesAttributionAndNamesItsSource(t *testing.T) {
	repoTop := newBridgeRepo(t)
	src := plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})

	head := mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))
	msg := gitT(t, repoTop, "log", "-1", "--format=%B", head)
	for _, want := range []string{"Candidate: cand-a1", "Bead: " + tbBeadA, bridgeSourceTrailer + ": " + src} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in replayed message:\n%s", want, msg)
		}
	}
}

// --- acceptance 5: neutral + input states --------------------------------

// TestPlanBridge_IntegrationAbsentIsNeutral: nothing has ever been promoted.
// Not an error, and — like loto sync — the ref must NOT be bootstrapped as a
// side effect of asking about it.
func TestPlanBridge_IntegrationAbsentIsNeutral(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	plan := mustPlan(t, repoTop)
	if plan.IntegrationSHA != "" || len(plan.Beads) != 0 {
		t.Errorf("plan = %+v, want empty", plan)
	}
	if refExists(t, repoTop, IntegrationRef) {
		t.Error("PlanBridge bootstrapped refs/loto/integration")
	}
}

// TestPlanBridge_NothingPendingWhenIntegrationMatchesMain.
func TestPlanBridge_NothingPendingWhenIntegrationMatchesMain(t *testing.T) {
	repoTop := newBridgeRepo(t)
	if got := mustPlan(t, repoTop); len(got.Beads) != 0 || len(got.Unattributed) != 0 {
		t.Errorf("plan = %+v, want nothing to bridge", got)
	}
}

// TestPlanBridge_BeadFilter narrows a run to one bead.
func TestPlanBridge_BeadFilter(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	plantPromoted(t, repoTop, tbBeadB, tbCandB1, map[string]string{tfFileB: tbB1})

	plan, err := PlanBridge(context.Background(), BridgeParams{RepoTop: repoTop, BeadID: tbBeadB})
	if err != nil {
		t.Fatalf("PlanBridge: %v", err)
	}
	if len(plan.Beads) != 1 || plan.Beads[0].BeadID != tbBeadB {
		t.Errorf("filtered plan = %+v, want only %s", plan.Beads, tbBeadB)
	}
}

func TestPlanBridge_RejectsBlankRepoTop(t *testing.T) {
	if _, err := PlanBridge(context.Background(), BridgeParams{}); !errors.Is(err, ErrBridgeInput) {
		t.Errorf("err = %v, want ErrBridgeInput", err)
	}
}

// TestPlanBridge_LeavesTheWorkingTreeAndHeadAlone: N agents share this
// checkout. The bridge is ref-and-object plumbing, and must prove it.
func TestPlanBridge_LeavesTheWorkingTreeAndHeadAlone(t *testing.T) {
	repoTop := newBridgeRepo(t)
	plantPromoted(t, repoTop, tbBeadA, tbCandA1, map[string]string{tfFileA: tbA1})
	writeFile(t, repoTop, tbFileC, "a peer's uncommitted edit\n")

	headBefore := gitT(t, repoTop, "rev-parse", "HEAD")
	statusBefore := gitT(t, repoTop, "status", "--porcelain")

	mustBuild(t, repoTop, beadIn(t, mustPlan(t, repoTop), tbBeadA))

	if got := gitT(t, repoTop, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved: %s -> %s", headBefore, got)
	}
	if got := gitT(t, repoTop, "status", "--porcelain"); got != statusBefore {
		t.Errorf("working tree or index changed:\n%s\n---\n%s", statusBefore, got)
	}
	if got, err := os.ReadFile(filepath.Join(repoTop, tbFileC)); err != nil || string(got) != "a peer's uncommitted edit\n" {
		t.Errorf("the peer's edit was disturbed: %q, %v", got, err)
	}
}

func TestBeadIDUsableAsRef(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want bool
	}{
		{"loto-ovno.8", true},
		{"cp-5b4", true},
		{"a_b", true},
		{"", false},
		{".hidden", false},
		{"-lead", false},
		{"trail.", false},
		{"a..b", false},
		{"has/slash", false},
		{"has space", false},
		{"x.lock", false},
	} {
		if got := beadIDUsableAsRef(tc.id); got != tc.want {
			t.Errorf("beadIDUsableAsRef(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}

// --- acceptance 6: against real promotion output --------------------------

// TestBridge_EndToEndFromPromote closes the loop the bead is about: real
// candidates, real Promote, then the bridge. If promotion's trailer shape
// ever drifts from what the bridge groups on, this is the test that says so.
func TestBridge_EndToEndFromPromote(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	idA, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)
	idB, _ := plantCandidate(t, repoTop, integration, tfFileB, "package gate\n\nvar B = 1\n", tpAgentB, tpBeadB)

	rec := &claimRecorder{}
	if _, err := Promote(context.Background(), promoteParams(repoTop, rec)); err != nil {
		t.Fatalf("Promote: %v", err)
	}

	plan := mustPlan(t, repoTop)
	if len(plan.Beads) != 2 || len(plan.Unattributed) != 0 {
		t.Fatalf("plan = %d beads / %d unattributed, want 2/0", len(plan.Beads), len(plan.Unattributed))
	}
	for _, bead := range []string{tpBeadA, tpBeadB} {
		b := beadIn(t, plan, bead)
		if b.Class != BridgeBuildable {
			t.Fatalf("%s: class = %q, want buildable", bead, b.Class)
		}
		head := mustBuild(t, repoTop, b)
		if got := countCommits(t, repoTop, tbMain+".."+head); got != 1 {
			t.Errorf("%s: branch carries %d commits, want 1", bead, got)
		}
	}
	// Attribution survives the crossing, and ONLY this bead's attribution
	// does. Promotion stamps its batch bookkeeping on whichever chain commit
	// happened to be the tip, and Batch-Candidates there names the OTHER
	// bead's candidate too — carrying that onto this branch would put an id in
	// this PR that is not in this PR. Which bead lands on the tip depends on
	// random candidate ids, so this assertion is also the one that catches a
	// regression here only sometimes unless the trailers are stripped.
	msgA := gitT(t, repoTop, "log", "--format=%B", tbMain+".."+beadIn(t, plan, tpBeadA).Ref)
	if !strings.Contains(msgA, idA) || strings.Contains(msgA, idB) {
		t.Errorf("bead A's branch should name %s and only %s:\n%s", idA, idA, msgA)
	}
	for _, unwanted := range []string{trailerCandidates, trailerHost, trailerPID, trailerProcStart, trailerBatch + ":"} {
		if strings.Contains(msgA, unwanted) {
			t.Errorf("promotion batch bookkeeping %q survived onto the branch:\n%s", unwanted, msgA)
		}
	}
}
