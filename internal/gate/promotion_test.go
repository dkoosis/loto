//go:build unix

package gate

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"loto/internal/domain"
)

// ‡ No t.Parallel anywhere in this file. promoteBeforePhase3Fn is a mutable
// package var, and under -race a parallel test that never touches it still
// races with one that does.

const (
	tpVerifyTrue = "true"
	tpBlob       = "blob"
	tpAgentA     = "agent-a"
	tpAgentB     = "agent-b"
	tpBeadA      = "loto-ovno.7a"
	tpBeadB      = "loto-ovno.7b"
)

// claimRecorder is the ClaimReleaser test double: records every candidate whose
// claims were released, and can be told to fail.
type claimRecorder struct {
	mu       sync.Mutex
	released []string
	fail     error
}

func (c *claimRecorder) ReleaseCandidateClaims(_ context.Context, candidateID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.released = append(c.released, candidateID)
	return c.fail
}

func (c *claimRecorder) saw(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Contains(c.released, id)
}

// liveProbeT/deadProbeT/unknownProbeT are the three liveness verdicts a
// promoting claim can get. Reclaim must happen on DEAD and on nothing else.
func liveProbeT(domain.LockRecord) domain.Liveness    { return domain.LivenessAlive }
func deadProbeT(domain.LockRecord) domain.Liveness    { return domain.LivenessDead }
func unknownProbeT(domain.LockRecord) domain.Liveness { return domain.LivenessUnknown }

// bootstrapIntegration plants refs/loto/integration at the fixture tip so the
// phase-1 snapshot is explicit rather than a bootstrap side effect.
func bootstrapIntegration(t *testing.T, repoTop, at string) {
	t.Helper()
	gitT(t, repoTop, "update-ref", IntegrationRef, at)
}

// plantCandidate does what admission does: lane-commit a change, capture its
// envelope against integration, write the envelope blob, and publish both refs.
func plantCandidate(t *testing.T, repoTop, integration, file, content, agent, bead string) (id string, env Envelope) {
	t.Helper()
	writeFile(t, repoTop, file, content)
	id = NewCandidateID()
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane-"+id, file))
	env = mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{file}, CandidateID: id,
		Agent: domain.AgentUUID(agent), Session: domain.SessionUUID(agent + "-s"), BeadID: bead,
	})
	blob, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if err := WriteCandidateRefs(context.Background(), repoTop, id, blob, proposal); err != nil {
		t.Fatalf("WriteCandidateRefs: %v", err)
	}
	// Restore the working tree: lane.Commit reads it, and every later
	// candidate in the same test must be captured against integration, not
	// against a tree still carrying this candidate's edit.
	gitT(t, repoTop, "checkout", "--", ".")
	return id, env
}

func promoteParams(repoTop string, rec *claimRecorder, verify ...string) PromoteParams {
	if len(verify) == 0 {
		verify = []string{tpVerifyTrue}
	}
	return PromoteParams{
		RepoTop: repoTop, VerifyCmd: verify, Claims: rec,
		Host: "h", PID: os.Getpid(), ProcStart: 12345,
	}
}

func outcomeFor(t *testing.T, res PromoteResult, id string) CandidateOutcome {
	t.Helper()
	for _, o := range res.Outcomes {
		if o.CandidateID == id {
			return o
		}
	}
	t.Fatalf("no outcome for %s in %+v", id, res.Outcomes)
	return CandidateOutcome{}
}

func refExists(t *testing.T, repoTop, ref string) bool {
	t.Helper()
	refs, err := listRefsWithSHAs(context.Background(), repoTop, ref)
	if err != nil {
		t.Fatal(err)
	}
	return len(refs) > 0
}

// --- acceptance 1: the happy path ---------------------------------------

// TestPromote_ThreePhaseHappyPath: two candidates on disjoint files land as two
// commits on integration, in candidate-ID order, each carrying its attribution;
// every ref the batch consumed is gone and both candidates' claims released.
func TestPromote_ThreePhaseHappyPath(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	idA, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)
	idB, _ := plantCandidate(t, repoTop, integration, tfFileB, "package gate\n\nvar B = 1\n", tpAgentB, tpBeadB)

	rec := &claimRecorder{}
	res, err := Promote(context.Background(), promoteParams(repoTop, rec))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	for _, id := range []string{idA, idB} {
		if got := outcomeFor(t, res, id).Class; got != OutcomePromoted {
			t.Errorf("%s: class = %q, want promoted", id, got)
		}
		if !rec.saw(id) {
			t.Errorf("%s: claims never released", id)
		}
		if refExists(t, repoTop, candidateRef(id)) {
			t.Errorf("%s: candidate ref survived promotion", id)
		}
		if refExists(t, repoTop, proposalRef(id)) {
			t.Errorf("%s: proposal ref survived promotion", id)
		}
	}
	if refExists(t, repoTop, promotingRefPrefix) {
		t.Error("a promoting claim survived a successful promotion")
	}

	tip := gitT(t, repoTop, "rev-parse", IntegrationRef)
	if res.Integration != tip {
		t.Errorf("result integration = %q, want %q", res.Integration, tip)
	}
	log := gitT(t, repoTop, "rev-list", integration+".."+tip)
	if got := len(strings.Split(log, "\n")); got != 2 {
		t.Fatalf("integration advanced by %d commits, want 2:\n%s", got, log)
	}
	// Sorted-ID order: the FIRST id is the parent, so it appears last in
	// rev-list's reverse-chronological output.
	wantOrder := []string{idA, idB}
	if idB < idA {
		wantOrder = []string{idB, idA}
	}
	msgs := gitT(t, repoTop, "log", "--format=%B", integration+".."+tip)
	firstAt, secondAt := strings.Index(msgs, wantOrder[0]), strings.Index(msgs, wantOrder[1])
	if firstAt < secondAt {
		t.Errorf("chain order is not by candidate id:\n%s", msgs)
	}
	for _, want := range []string{"Agent: " + tpAgentA, "Bead: " + tpBeadA, "Agent: " + tpAgentB, "Bead: " + tpBeadB} {
		if !strings.Contains(msgs, want) {
			t.Errorf("missing attribution trailer %q in:\n%s", want, msgs)
		}
	}
	// The promoted trees must carry both changes and nothing else.
	if got := gitT(t, repoTop, "show", tip+":"+tfFileB); !strings.Contains(got, "var B = 1") {
		t.Errorf("b.go content did not land: %q", got)
	}
	if got := gitT(t, repoTop, "show", tip+":"+tfFileA); !strings.Contains(got, "var A = 2") {
		t.Errorf("a.go content did not land: %q", got)
	}
}

// --- acceptance 2: the red-batch rule -----------------------------------

// TestPromote_RedBatchAttributesFailingCandidate: one bad candidate must not
// take an innocent one down with it. The batch chain contains red.go so the
// batch verify fails; A's solo chain does not (green, promotes); B's solo chain
// does (red, rejected with attribution).
func TestPromote_RedBatchAttributesFailingCandidate(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	idA, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)
	idB, _ := plantCandidate(t, repoTop, integration, "red.go", "package gate\n", tpAgentB, tpBeadB)

	rec := &claimRecorder{}
	p := promoteParams(repoTop, rec, "sh", "-c", "! test -e red.go")
	res, err := Promote(context.Background(), p)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}

	if got := outcomeFor(t, res, idA).Class; got != OutcomePromoted {
		t.Errorf("innocent candidate %s: class = %q, want promoted", idA, got)
	}
	bad := outcomeFor(t, res, idB)
	if bad.Class != OutcomeVerifyRed {
		t.Fatalf("red candidate %s: class = %q, want verify-red", idB, bad.Class)
	}
	if bad.Agent != tpAgentB || bad.BeadID != tpBeadB {
		t.Errorf("rejection must name its author: agent=%q bead=%q", bad.Agent, bad.BeadID)
	}
	if refExists(t, repoTop, candidateRef(idB)) || refExists(t, repoTop, proposalRef(idB)) {
		t.Error("a rejected candidate's refs must be retired")
	}
	if !rec.saw(idB) {
		t.Error("a rejected candidate's claims must be released")
	}
	if refExists(t, repoTop, promotingRefPrefix) {
		t.Error("a promoting claim survived the red-batch sweep")
	}

	tip := gitT(t, repoTop, "rev-parse", IntegrationRef)
	if n := len(strings.Split(gitT(t, repoTop, "rev-list", integration+".."+tip), "\n")); n != 1 {
		t.Errorf("integration advanced by %d commits, want 1 (A only)", n)
	}
	if err := gitCatFileFails(t, repoTop, tip, "red.go"); err == nil {
		t.Error("red.go must not be on integration")
	}
}

func gitCatFileFails(t *testing.T, repoTop, commit, path string) error {
	t.Helper()
	g := gitRunner{repoTop: repoTop}
	_, err := g.run(context.Background(), "cat-file", "-e", commit+":"+path)
	return err
}

// --- acceptance 3: the CAS ----------------------------------------------

// TestPromote_SnapshotMovedRequeues: integration moving between phase 1 and
// phase 3 must lose the CAS and requeue the candidate untouched — the whole
// reason every transaction line carries an OldSHA.
func TestPromote_SnapshotMovedRequeues(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	// Advance integration once, out of band, on a path the candidate does not
	// touch — so only the CAS, not staleness, can reject it.
	var once sync.Once
	moveIntegration := func() {
		once.Do(func() {
			writeFile(t, repoTop, "unrelated.go", "package gate\n")
			gitT(t, repoTop, "add", "unrelated.go")
			gitT(t, repoTop, "commit", "-qm", "out of band")
			gitT(t, repoTop, "update-ref", IntegrationRef, gitT(t, repoTop, "rev-parse", "HEAD"))
		})
	}
	promoteBeforePhase3Fn = moveIntegration
	t.Cleanup(func() { promoteBeforePhase3Fn = nil })

	rec := &claimRecorder{}
	p := promoteParams(repoTop, rec)
	p.MaxRounds = 1
	res, err := Promote(context.Background(), p)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := outcomeFor(t, res, id).Class; got != OutcomePromotionRace {
		t.Fatalf("class = %q, want promotion-race", got)
	}
	if !refExists(t, repoTop, candidateRef(id)) || !refExists(t, repoTop, proposalRef(id)) {
		t.Error("a requeued candidate keeps its refs")
	}
	if rec.saw(id) {
		t.Error("a requeued candidate must not have its claims released")
	}
	if refExists(t, repoTop, promotingRefPrefix) {
		t.Error("the promoting claim must be dropped when the CAS loses")
	}

	t.Run("second round rebuilds on the moved snapshot", func(t *testing.T) {
		p := promoteParams(repoTop, rec)
		p.MaxRounds = 2
		res, err := Promote(context.Background(), p)
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if got := outcomeFor(t, res, id).Class; got != OutcomePromoted {
			t.Fatalf("class = %q, want promoted on the retry", got)
		}
	})
}

// TestPromotePhase3NonCASFailureIsNotErrPromoteRace pins the loto-in5f fix:
// promotePhase3 must route a non-CAS UpdateRefsTx failure (an exec-level
// error, here a batch chain tip that names a git object which does not
// exist) to an infra-error path, never through errPromoteRace — that
// sentinel is reserved for a genuine lost compare-and-swap
// (TestPromote_SnapshotMovedRequeues covers that side). Before the fix, every
// UpdateRefsTx failure was blanket-wrapped in errPromoteRace regardless of
// cause.
func TestPromotePhase3NonCASFailureIsNotErrPromoteRace(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	p := promoteParams(repoTop, &claimRecorder{})
	b := &batch{
		id:       "b-nonexistent-object",
		snapshot: integration,
		// A well-formed but nonexistent object SHA: git rejects this with
		// "trying to write ref ... with nonexistent object", never the
		// CAS-rejection wording ("... is at ... but expected ...").
		chainTip: strings.Repeat("2", 40),
	}

	err := promotePhase3(context.Background(), p, b)
	if err == nil {
		t.Fatal("promotePhase3: want an error updating integration to a nonexistent object, got nil")
	}
	if errors.Is(err, errPromoteRace) {
		t.Errorf("a non-CAS infra failure must not classify as errPromoteRace: %v", err)
	}
	if !strings.Contains(err.Error(), "nonexistent object") {
		t.Errorf("err = %v, want the underlying git failure surfaced, not swallowed", err)
	}
}

// --- acceptance 4: reclaim ----------------------------------------------

// TestPromote_ReclaimsStalePromotingClaim: a pusher that died mid-verify leaves
// a promoting ref behind. A later pusher reclaims it — but only on a positive
// DEAD verdict.
func TestPromote_ReclaimsStalePromotingClaim(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	// A crashed pusher's leftovers: a chain commit carrying its claim trailers
	// and a promoting ref pointing at it.
	plantStale := func(t *testing.T) string {
		t.Helper()
		batchID := newBatchID()
		msg := strings.Join([]string{
			"loto: promote " + id, "",
			"Candidate: " + id,
			trailerBatch + ": " + batchID,
			trailerCandidates + ": " + id,
			trailerHost + ": h",
			trailerPID + ": 4194304",
			trailerProcStart + ": 12345",
		}, "\n") + "\n"
		tree := gitT(t, repoTop, "rev-parse", integration+"^{tree}")
		commit := gitT(t, repoTop, "commit-tree", tree, "-p", integration, "-m", msg)
		gitT(t, repoTop, "update-ref", promotingRef(batchID), commit)
		return batchID
	}

	batchID := plantStale(t)
	rec := &claimRecorder{}

	t.Run("unknown liveness never reclaims", func(t *testing.T) {
		p := promoteParams(repoTop, rec)
		p.Live = unknownProbeT
		res, err := Promote(context.Background(), p)
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if !refExists(t, repoTop, promotingRef(batchID)) {
			t.Error("UNKNOWN must leave a peer's claim standing")
		}
		if len(res.Outcomes) != 0 {
			t.Errorf("a claimed candidate must not be promoted: %+v", res.Outcomes)
		}
	})

	t.Run("nil probe never reclaims", func(t *testing.T) {
		res, err := Promote(context.Background(), promoteParams(repoTop, rec))
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if !refExists(t, repoTop, promotingRef(batchID)) {
			t.Error("a nil probe must leave a peer's claim standing")
		}
		if len(res.Outcomes) != 0 {
			t.Errorf("a claimed candidate must not be promoted: %+v", res.Outcomes)
		}
	})

	t.Run("alive never reclaims", func(t *testing.T) {
		p := promoteParams(repoTop, rec)
		p.Live = liveProbeT
		if _, err := Promote(context.Background(), p); err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if !refExists(t, repoTop, promotingRef(batchID)) {
			t.Error("a live pusher's claim must stand")
		}
	})

	t.Run("dead reclaims and promotes in the same call", func(t *testing.T) {
		p := promoteParams(repoTop, rec)
		p.Live = deadProbeT
		res, err := Promote(context.Background(), p)
		if err != nil {
			t.Fatalf("Promote: %v", err)
		}
		if refExists(t, repoTop, promotingRef(batchID)) {
			t.Error("a dead pusher's claim must be reclaimed")
		}
		if got := outcomeFor(t, res, id).Class; got != OutcomePromoted {
			t.Errorf("class = %q, want promoted once the claim is reclaimed", got)
		}
	})
}

// TestPromote_UndecodablePromotingClaimBlocksReselection pins the loto-fxja
// fix: a promoting ref whose PID trailer will not parse must still keep its
// candidate spoken for. Before the fix, reclaimDeadPromotions hit the decode
// error, ran a bare continue with nothing added to the claimed map, and
// selectCandidates re-picked id into a fresh batch — promoting it a second
// time while the original (unreadable but real) promoting ref for it still
// stood.
func TestPromote_UndecodablePromotingClaimBlocksReselection(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	// A promoting ref whose PID trailer is not numeric — readPromotionClaim
	// fails to parse it before it ever gets to Batch-Candidates in the old
	// code, but must still name id in the (fail-safe, error-returning) result.
	batchID := newBatchID()
	msg := strings.Join([]string{
		"loto: promote " + id, "",
		"Candidate: " + id,
		trailerBatch + ": " + batchID,
		trailerCandidates + ": " + id,
		trailerHost + ": h",
		trailerPID + ": not-a-pid",
		trailerProcStart + ": 12345",
	}, "\n") + "\n"
	tree := gitT(t, repoTop, "rev-parse", integration+"^{tree}")
	commit := gitT(t, repoTop, "commit-tree", tree, "-p", integration, "-m", msg)
	gitT(t, repoTop, "update-ref", promotingRef(batchID), commit)

	rec := &claimRecorder{}
	p := promoteParams(repoTop, rec)
	p.Live = deadProbeT // irrelevant: an undecodable claim never reaches the liveness check
	res, err := Promote(context.Background(), p)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("id must not be re-selected while its unreadable promoting claim stands: %+v", res.Outcomes)
	}
	if !refExists(t, repoTop, promotingRef(batchID)) {
		t.Error("an undecodable claim must be left alone, not deleted")
	}
}

// --- supporting units ---------------------------------------------------

// TestBuildChain_AppliesCreateModifyDelete pins the transition triple: the
// chain applies Transitions, not a cherry-picked proposal diff, so a creation,
// a modification, and a deletion must all land — the deletion through
// update-index's mode-0 syntax.
func TestBuildChain_AppliesCreateModifyDelete(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	// integration carries go.mod + a.go; add doomed.go so we have something to
	// delete, and re-point integration at it.
	writeFile(t, repoTop, "doomed.go", "package gate\n")
	gitT(t, repoTop, "add", "doomed.go")
	gitT(t, repoTop, "commit", "-qm", "add doomed")
	integration = gitT(t, repoTop, "rev-parse", "HEAD")
	gitT(t, repoTop, "update-ref", IntegrationRef, integration)

	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 9\n") // modify
	writeFile(t, repoTop, "made.go", "package gate\n")            // create
	if err := os.Remove(filepath.Join(repoTop, "doomed.go")); err != nil {
		t.Fatal(err)
	}
	id := NewCandidateID()
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane-"+id, tfFileA, "made.go", "doomed.go"))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal, Base: integration,
		WriteSet: []string{tfFileA, "made.go", "doomed.go"}, CandidateID: id,
		Agent: tpAgentA, Session: tpAgentA + "-s", BeadID: tpBeadA,
	})

	b := &batch{id: newBatchID(), snapshot: integration, members: []member{{candidateID: id, env: env, proposalSHA: proposal}}}
	tip, err := buildChain(context.Background(), promoteParams(repoTop, &claimRecorder{}), b)
	if err != nil {
		t.Fatalf("buildChain: %v", err)
	}

	if got := gitT(t, repoTop, "show", tip+":"+tfFileA); !strings.Contains(got, "var A = 9") {
		t.Errorf("modify did not land: %q", got)
	}
	if got := gitT(t, repoTop, "cat-file", "-t", tip+":made.go"); got != tpBlob {
		t.Errorf("create did not land: %q", got)
	}
	if err := gitCatFileFails(t, repoTop, tip, "doomed.go"); err == nil {
		t.Error("delete did not land: doomed.go still present")
	}
	// Everything else in integration is preserved — the theorem's other half.
	if got := gitT(t, repoTop, "cat-file", "-t", tip+":go.mod"); got != tpBlob {
		t.Errorf("unrelated path lost: %q", got)
	}
	if parent := gitT(t, repoTop, "rev-parse", tip+"^"); parent != integration {
		t.Errorf("chain parent = %q, want the snapshot %q", parent, integration)
	}
}

// TestBuildChain_IgnoresCallerGitIdentityEnv pins the loto-nnqd fix: the chain
// commit's identity must stay loto-gate even when the invoking shell exports
// its own GIT_AUTHOR_NAME/EMAIL and GIT_COMMITTER_NAME/EMAIL. cmd.Env used to
// be built promoteIdentityEnv() first, os.Environ() second — os/exec (and the
// OS beneath it) resolve a duplicated env key to its LAST occurrence, so the
// shell's values silently won.
func TestBuildChain_IgnoresCallerGitIdentityEnv(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, env := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	t.Setenv("GIT_AUTHOR_NAME", "someone-else")
	t.Setenv("GIT_AUTHOR_EMAIL", "someone-else@shell")
	t.Setenv("GIT_COMMITTER_NAME", "someone-else")
	t.Setenv("GIT_COMMITTER_EMAIL", "someone-else@shell")

	b := &batch{id: newBatchID(), snapshot: integration, members: []member{{candidateID: id, env: env}}}
	tip, err := buildChain(context.Background(), promoteParams(repoTop, &claimRecorder{}), b)
	if err != nil {
		t.Fatalf("buildChain: %v", err)
	}

	if got := gitT(t, repoTop, "log", "-1", "--format=%an <%ae>", tip); got != "loto-gate <loto@localhost>" {
		t.Errorf("author = %q, want the loto-gate identity even with the shell's env exported", got)
	}
	if got := gitT(t, repoTop, "log", "-1", "--format=%cn <%ce>", tip); got != "loto-gate <loto@localhost>" {
		t.Errorf("committer = %q, want the loto-gate identity even with the shell's env exported", got)
	}
}

// TestPromote_SkipsStalePreimageCandidate pins D3: a candidate that no longer
// applies is SKIPPED, never rejected. Its refs and claims must survive.
func TestPromote_SkipsStalePreimageCandidate(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	// Move integration's copy of a.go out from under the candidate.
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 99\n")
	gitT(t, repoTop, "add", tfFileA)
	gitT(t, repoTop, "commit", "-qm", "peer changed a.go")
	gitT(t, repoTop, "update-ref", IntegrationRef, gitT(t, repoTop, "rev-parse", "HEAD"))
	gitT(t, repoTop, "checkout", "--", ".")

	rec := &claimRecorder{}
	res, err := Promote(context.Background(), promoteParams(repoTop, rec))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	o := outcomeFor(t, res, id)
	if o.Class != OutcomeStalePreimage {
		t.Fatalf("class = %q, want stale-preimage", o.Class)
	}
	if o.Detail == "" || !strings.Contains(o.Detail, tfFileA) {
		t.Errorf("detail must name the path: %q", o.Detail)
	}
	if !refExists(t, repoTop, candidateRef(id)) || !refExists(t, repoTop, proposalRef(id)) {
		t.Error("a skipped candidate keeps its refs — skipping is not rejecting")
	}
	if rec.saw(id) {
		t.Error("a skipped candidate must not have its claims released")
	}
}

// TestPromote_VerifyInfraRequeues: a verify that cannot RUN is not a failing
// candidate. Nothing may be rejected on infrastructure trouble.
func TestPromote_VerifyInfraRequeues(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	rec := &claimRecorder{}
	res, err := Promote(context.Background(), promoteParams(repoTop, rec, "loto-no-such-binary-9f2a"))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := outcomeFor(t, res, id).Class; got != OutcomeVerifyInfra {
		t.Fatalf("class = %q, want verify-infrastructure", got)
	}
	if !refExists(t, repoTop, candidateRef(id)) {
		t.Error("infrastructure trouble must never retire a candidate")
	}
	if rec.saw(id) {
		t.Error("infrastructure trouble must never release claims")
	}
	if refExists(t, repoTop, promotingRefPrefix) {
		t.Error("the promoting claim must be dropped when verify cannot run")
	}
}

// TestPromote_RejectsBadInput pins the caller-bug guards.
func TestPromote_RejectsBadInput(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	rec := &claimRecorder{}
	for _, tc := range []struct {
		name string
		p    PromoteParams
	}{
		{"no repo", PromoteParams{VerifyCmd: []string{tpVerifyTrue}, Claims: rec}},
		{"no verify command", PromoteParams{RepoTop: repoTop, Claims: rec}},
		{"no claim releaser", PromoteParams{RepoTop: repoTop, VerifyCmd: []string{tpVerifyTrue}}},
	} {
		if _, err := Promote(context.Background(), tc.p); !errors.Is(err, errPromoteInput) {
			t.Errorf("%s: err = %v, want errPromoteInput", tc.name, err)
		}
	}
}

// TestPromote_EmptyQueueIsNotAnError: nothing to promote is an ordinary state.
func TestPromote_EmptyQueueIsNotAnError(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	res, err := Promote(context.Background(), promoteParams(repoTop, &claimRecorder{}))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("outcomes = %+v, want none", res.Outcomes)
	}
	if res.Integration != integration {
		t.Errorf("integration = %q, want it unmoved at %q", res.Integration, integration)
	}
}

// TestPromote_MaxBatchBoundsOneRound pins the batch cap, which is what bounds
// the red-batch fallback's worst case.
func TestPromote_MaxBatchBoundsOneRound(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	for _, f := range []string{"one.go", "two.go", "three.go"} {
		plantCandidate(t, repoTop, integration, f, "package gate\n", tpAgentA, tpBeadA)
	}

	p := promoteParams(repoTop, &claimRecorder{})
	p.MaxBatch = 1
	p.MaxRounds = 1
	res, err := Promote(context.Background(), p)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Outcomes) != 1 {
		t.Fatalf("one round with MaxBatch=1 promoted %d candidates, want 1", len(res.Outcomes))
	}
	if n := len(strings.Split(gitT(t, repoTop, "rev-list", integration+".."+res.Integration), "\n")); n != 1 {
		t.Errorf("integration advanced by %d commits, want 1", n)
	}
}

// TestParseTrailers_LastWins pins the trailer reader against git's own
// semantics, since the promoting claim is read back out of a commit message.
func TestParseTrailers_LastWins(t *testing.T) {
	got := parseTrailers("subject\n\nCandidate: c-1\nPromoter-PID: 7\nPromoter-PID: 9\n")
	if got["Candidate"] != "c-1" {
		t.Errorf("Candidate = %q", got["Candidate"])
	}
	if got[trailerPID] != "9" {
		t.Errorf("%s = %q, want the last occurrence", trailerPID, got[trailerPID])
	}
}

// TestPromoteFlock_SerializesPushers: the flock is what stops two pushers from
// building chains over the same candidates at the same time.
func TestPromoteFlock_SerializesPushers(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	ctx := context.Background()
	first, err := acquirePromoteFlock(ctx, repoTop)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	held := make(chan struct{})
	go func() {
		defer close(held)
		second, err := acquirePromoteFlock(ctx, repoTop)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		second.release()
	}()
	select {
	case <-held:
		t.Fatal("the second pusher took the flock while the first held it")
	default:
	}
	first.release()
	<-held
}

// TestPromote_VerifyRunsUnlocked pins the phase-2 contract: admission must keep
// running while a promotion verifies, which is only true if the flock is NOT
// held across verify.
//
// ‡ The probe runs DURING verify, not after it. A check that only fires once
// verify has returned would pass even if phase 2 held the lock the whole time —
// which is the bug this test exists to catch.
func TestPromote_VerifyRunsUnlocked(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	plantCandidate(t, repoTop, integration, tfFileA, "package gate\n\nvar A = 2\n", tpAgentA, tpBeadA)

	tmp := t.TempDir()
	marker := filepath.Join(tmp, "verifying")
	script := filepath.Join(tmp, "verify.sh")
	// Announce that verify is running, stay running long enough to be probed.
	body := "#!/bin/sh\ntouch " + marker + "\nsleep 2\nexit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	tookIt := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(marker); err == nil {
				break
			}
			if time.Now().After(deadline) {
				tookIt <- false
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		// Verify is in flight right now. A peer must be able to take the
		// promotion flock.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		h, err := acquirePromoteFlock(ctx, repoTop)
		if err != nil {
			tookIt <- false
			return
		}
		h.release()
		tookIt <- true
	}()

	if _, err := Promote(context.Background(), promoteParams(repoTop, &claimRecorder{}, script)); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if !<-tookIt {
		t.Error("the promotion flock was held across phase 2 — admission would have stalled behind verify")
	}
}

// TestPromote_MalformedEnvelopeIsNotPromoted is the loto-amkj regression at the
// layer that would have done the damage. Before validation, a candidate ref
// pointing at `{}` was selected, contributed no transitions, and still got a
// chain commit — integration advanced by an empty, unattributed commit and the
// candidate ref was retired as though it had landed.
func TestPromote_MalformedEnvelopeIsNotPromoted(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	// A candidate ref whose blob parses as JSON but carries nothing.
	id := NewCandidateID()
	empty := writeBlobT(t, repoTop, "{}\n")
	gitT(t, repoTop, "update-ref", candidateRef(id), empty)
	gitT(t, repoTop, "update-ref", proposalRef(id), integration)

	rec := &claimRecorder{}
	res, err := Promote(context.Background(), promoteParams(repoTop, rec))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if len(res.Outcomes) != 0 {
		t.Errorf("a malformed candidate must not produce an outcome: %+v", res.Outcomes)
	}
	if res.Integration != integration {
		t.Errorf("integration moved to %q — an empty commit was promoted", res.Integration)
	}
	if !refExists(t, repoTop, candidateRef(id)) {
		t.Error("a malformed candidate's ref must not be retired by promotion")
	}
	if rec.saw(id) {
		t.Error("a malformed candidate's claims must not be released")
	}
}

// writeBlobT writes content as a git blob and returns its SHA.
func writeBlobT(t *testing.T, repoTop, content string) string {
	t.Helper()
	cmd := exec.Command("git", "hash-object", "-w", "--stdin")
	cmd.Dir = repoTop
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hash-object: %v", err)
	}
	return strings.TrimSpace(string(out))
}
