//go:build unix

package gate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"loto/internal/domain"
)

// The git-gate contract suite (loto-ovno.11). Every test here pins one line of
// git-gate.md's "Verification criteria" list that no other test in this
// package already owns. The ones that ARE owned elsewhere stay there rather
// than being restated: diff ≠ write-set, stale epoch, stale preimage, and
// ancestry violation live in admission_test.go; the deletion CAPTURE
// round-trip and the promotion-claim reclaim live in envelope_test.go and
// promotion_test.go.
//
// ‡ No t.Parallel here either — same reason promotion_test.go gives.

const (
	tcNakedBead   = "loto-ovno.11-naked"
	tcCrashBead   = "loto-ovno.11-crash"
	tcConcurBead  = "loto-ovno.11-concurrent"
	tcDoomedFile  = "doomed.go"
	tcPeerContent = "package gate\n\nvar A = 2\n"
)

// --- naked candidate -------------------------------------------------------

// TestAdmit_RejectsNakedCandidate pins the fail-closed posture git-gate.md
// states as "a candidate ref without its envelope is rejected". Admission is
// handed a real proposal commit and NO envelope — the zero value — which is
// what a garbled or half-written acceptance leaves behind.
//
// ‡ Why this matters beyond the obvious: an empty envelope makes every
// downstream check VACUOUS. checkDiffMatchesWriteSet compares against an empty
// declared set, checkLeaseEpochs iterates an empty write-set, and both
// transition and ancestry loops have nothing to walk. If admission ever
// reordered its checks so the envelope's own identity were not established
// first, "no envelope" would read as "nothing to object to" and the naked
// candidate would be admitted by default. The contract is that no path through
// Admit returns Accepted for an envelope that names nothing.
func TestAdmit_RejectsNakedCandidate(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, tcPeerContent)
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	d, err := Admit(context.Background(), Envelope{}, AdmitParams{
		RepoTop: repoTop, PresentedProposalSHA: proposal, IntegrationRef: integration,
	})
	// Either verdict shape is acceptable — a rejection or a plumbing error on
	// the empty ProposalSHA. What is NOT acceptable is Accepted.
	if err == nil && d.Accepted {
		t.Fatal("an envelope-less candidate must never be admitted")
	}
}

// TestPromote_NakedCandidateIsNeverPromoted pins the same contract one layer
// down, where it is actually reachable: refs/loto/candidates/<id> exists but
// the envelope behind it does not resolve. Promotion must leave that candidate
// alone and keep draining the rest of the queue — a naked ref is residue for
// doctor to clear, not a reason to wedge every healthy candidate behind it.
//
// ‡ The healthy peer in each subtest is load-bearing. Without it, "integration
// did not advance" would also pass if promotion had crashed, refused to run,
// or found no refs at all. The peer proves the round RAN and made progress,
// and that exactly the naked candidate was the thing left out.
func TestPromote_NakedCandidateIsNeverPromoted(t *testing.T) {
	t.Run("envelope blob does not decode", func(t *testing.T) {
		repoTop, integration := newIntegrationRepo(t)
		bootstrapIntegration(t, repoTop, integration)
		good, _ := plantCandidate(t, repoTop, integration, tfFileA, tcPeerContent, tpAgentA, tcNakedBead)
		naked := plantNakedCandidate(t, repoTop, integration, "this is not an envelope\n")

		res, err := Promote(context.Background(), promoteParams(repoTop, &claimRecorder{}))
		if err != nil {
			t.Fatalf("a naked candidate must not fail the whole round: %v", err)
		}
		assertNakedSkipped(t, repoTop, res, integration, good, naked)
	})

	t.Run("candidate ref with no proposal ref", func(t *testing.T) {
		repoTop, integration := newIntegrationRepo(t)
		bootstrapIntegration(t, repoTop, integration)
		good, _ := plantCandidate(t, repoTop, integration, tfFileA, tcPeerContent, tpAgentA, tcNakedBead)
		naked, _ := plantCandidate(t, repoTop, integration, tfFileB, "package gate\n\nvar B = 1\n", tpAgentB, tcNakedBead)
		// Half-written acceptance: the envelope landed, the proposal ref did
		// not. Nothing keeps the proposal commit reachable, so promoting on the
		// strength of the envelope alone would build a chain over an object git
		// is free to collect.
		gitT(t, repoTop, "update-ref", "-d", proposalRef(naked))

		res, err := Promote(context.Background(), promoteParams(repoTop, &claimRecorder{}))
		if err != nil {
			t.Fatalf("a proposal-less candidate must not fail the whole round: %v", err)
		}
		assertNakedSkipped(t, repoTop, res, integration, good, naked)
	})
}

// plantNakedCandidate writes a candidates ref pointing at a blob that is not an
// envelope, plus a proposal ref, and returns the candidate id.
func plantNakedCandidate(t *testing.T, repoTop, proposal, body string) string {
	t.Helper()
	id := NewCandidateID()
	blob := gitHashObject(t, repoTop, body)
	if err := WriteCandidateRefs(context.Background(), repoTop, id, blob, proposal); err != nil {
		t.Fatalf("plant naked candidate: %v", err)
	}
	return id
}

func gitHashObject(t *testing.T, repoTop, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "blob")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return gitT(t, repoTop, "hash-object", "-w", path)
}

// assertNakedSkipped is the shared verdict: the healthy candidate promoted, the
// naked one got no outcome at all, its refs survive untouched, and integration
// advanced by exactly the one healthy commit.
func assertNakedSkipped(t *testing.T, repoTop string, res PromoteResult, integration, good, naked string) {
	t.Helper()
	if got := outcomeFor(t, res, good).Class; got != OutcomePromoted {
		t.Errorf("the healthy candidate must still promote, got class %q", got)
	}
	for _, o := range res.Outcomes {
		if o.CandidateID == naked {
			t.Errorf("a naked candidate must produce no outcome, got %+v", o)
		}
	}
	if !refExists(t, repoTop, candidateRef(naked)) {
		t.Error("a naked candidate's ref must survive — skipping is not retiring")
	}
	tip := gitT(t, repoTop, "rev-parse", IntegrationRef)
	if n := len(strings.Split(gitT(t, repoTop, "rev-list", integration+".."+tip), "\n")); n != 1 {
		t.Errorf("integration advanced by %d commits, want exactly 1 (the healthy candidate)", n)
	}
}

// --- deletion round-trip ---------------------------------------------------

// TestPromote_DeletionRoundTripsToIntegration walks a DELETION the whole way:
// lane.Commit → Capture → Admit → refs → Promote → integration. envelope_test
// pins the capture half (Result == nil) and promotion_test pins the chain half
// (buildChain's mode-0 removal syntax); neither shows the two halves agreeing
// through admission, which is the step with the most ways to disagree about
// absence — diff-tree reports a deleted path as changed, blobAt reports it as
// (nil, nil), and the transition CAS compares a nil against a nil.
//
// The preserved siblings are half the promotion theorem: "promotion changes
// exactly those paths and preserves all other integration state."
func TestPromote_DeletionRoundTripsToIntegration(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	writeFile(t, repoTop, tcDoomedFile, "package gate\n\nvar Doomed = 1\n")
	gitT(t, repoTop, "add", tcDoomedFile)
	gitT(t, repoTop, "commit", "-qm", "add doomed")
	integration := gitT(t, repoTop, "rev-parse", "HEAD")
	bootstrapIntegration(t, repoTop, integration)

	if err := os.Remove(filepath.Join(repoTop, tcDoomedFile)); err != nil {
		t.Fatal(err)
	}
	id := NewCandidateID()
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane-"+id, tcDoomedFile))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcDoomedFile}, CandidateID: id,
		Agent: tpAgentA, Session: tpAgentA + "-s", BeadID: tfBead,
	})

	// Admission must accept a pure deletion, not mistake absence for a
	// malformed transition.
	if d := mustAdmit(t, env, admitParamsFor(repoTop, env, integration)); !d.Accepted {
		t.Fatalf("a deletion must be admissible: reason=%s detail=%s", d.Reason, d.Detail)
	}

	blob, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteCandidateRefs(context.Background(), repoTop, id, blob, proposal); err != nil {
		t.Fatal(err)
	}

	rec := &claimRecorder{}
	res, err := Promote(context.Background(), promoteParams(repoTop, rec))
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if got := outcomeFor(t, res, id).Class; got != OutcomePromoted {
		t.Fatalf("class = %q, want promoted", got)
	}
	tip := gitT(t, repoTop, "rev-parse", IntegrationRef)
	if err := gitCatFileFails(t, repoTop, tip, tcDoomedFile); err == nil {
		t.Error("the deleted path is still present on integration")
	}
	for _, keep := range []string{tfFileA, "go.mod"} {
		if got := gitT(t, repoTop, "cat-file", "-t", tip+":"+keep); got != tpBlob {
			t.Errorf("%s: unrelated integration state was not preserved (%q)", keep, got)
		}
	}
	if !rec.saw(id) {
		t.Error("a promoted deletion must release its claims like any other candidate")
	}
}

// --- crash between phases --------------------------------------------------

// TestPromote_CrashBetweenPhasesLeavesRefsConsistent is git-gate.md's
// "after a crash, refs alone reconstruct state": a pusher dies in the window
// between verify (phase 2) and the ref transaction (phase 3).
//
// The crash is modeled by cancelling the promotion's context at exactly that
// boundary, which is what a killed process looks like from the refs' side —
// phase 3 never runs, and nothing gets to clean up after it. What must survive
// is a CONSISTENT world: integration unmoved, the candidate still owning its
// refs, and one promoting claim naming the batch. Then the contract's second
// half: the NEXT pusher drains it, once the dead promoter's claim is provably
// reclaimable.
//
// ‡ The two halves have to be one test. Asserting only the leftovers would not
// show the state is recoverable, and asserting only the drain (which
// promotion_test does with a hand-planted claim) would not show that a real
// crash leaves the shape the reclaim path expects.
func TestPromote_CrashBetweenPhasesLeavesRefsConsistent(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)
	id, _ := plantCandidate(t, repoTop, integration, tfFileA, tcPeerContent, tpAgentA, tcCrashBead)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	promoteBeforePhase3Fn = cancel
	t.Cleanup(func() { promoteBeforePhase3Fn = nil })

	p := promoteParams(repoTop, &claimRecorder{})
	p.MaxRounds = 1
	if _, err := Promote(ctx, p); err == nil {
		t.Fatal("a pusher killed before phase 3 must not report success")
	}
	promoteBeforePhase3Fn = nil

	// 1. Integration never moved: the transaction is all-or-nothing and it
	//    never ran.
	if got := gitT(t, repoTop, "rev-parse", IntegrationRef); got != integration {
		t.Errorf("integration = %s, want it unmoved at %s", got, integration)
	}
	// 2. The candidate still owns its refs, so nothing about it was consumed.
	if !refExists(t, repoTop, candidateRef(id)) || !refExists(t, repoTop, proposalRef(id)) {
		t.Fatal("a crashed promotion must leave its candidate's refs intact")
	}
	// 3. Exactly one promoting claim survives, and it names this candidate —
	//    the durable record that says which work was in flight and whose.
	claims, err := listRefsWithSHAs(context.Background(), repoTop, promotingRefPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("want exactly 1 leftover promoting claim, got %d: %v", len(claims), claims)
	}
	for batchID, tip := range claims {
		cl, err := readPromotionClaim(context.Background(), repoTop, tip)
		if err != nil {
			t.Fatalf("the leftover claim must still be readable — it is how the next pusher decides: %v", err)
		}
		if len(cl.candidates) != 1 || cl.candidates[0] != id {
			t.Errorf("claim %s names %v, want [%s]", batchID, cl.candidates, id)
		}
	}

	// 4. The next pusher drains it. deadProbeT is the positive liveness verdict
	//    on the crashed promoter that reclaim requires — nothing weaker.
	rec := &claimRecorder{}
	next := promoteParams(repoTop, rec)
	next.Live = deadProbeT
	res, err := Promote(context.Background(), next)
	if err != nil {
		t.Fatalf("the next pusher must drain the queue: %v", err)
	}
	if got := outcomeFor(t, res, id).Class; got != OutcomePromoted {
		t.Fatalf("class = %q, want promoted by the next pusher", got)
	}
	if refExists(t, repoTop, promotingRefPrefix) {
		t.Error("the dead pusher's promoting claim must be gone once the batch lands")
	}
	if got := gitT(t, repoTop, "show", gitT(t, repoTop, "rev-parse", IntegrationRef)+":"+tfFileA); !strings.Contains(got, "var A = 2") {
		t.Errorf("the drained candidate's content is not on integration: %q", got)
	}
}

// --- concurrent submits ----------------------------------------------------

// TestConcurrentAcceptance_SerializesWithoutLoss pins "concurrent submits
// serialize". Four candidates on four disjoint paths are accepted from four
// goroutines at once — the git-side half of `loto submit`: capture the
// envelope, hash it into the object store, and publish both refs in one
// update-ref transaction.
//
// Two things have to hold, and only concurrency can test either. Every
// acceptance lands WHOLE — no candidate ends up with an envelope ref and no
// proposal ref, which is precisely the half-written shape the naked-candidate
// test above shows promotion refuses to touch. And no acceptance is LOST: ref
// transactions racing on the same loose-ref directory must all be visible
// afterwards, not silently overwritten by whichever wrote last.
//
// ‡ The store-side half — lease revalidation under the op-flock — is store's
// own contract (internal/store/admission_test.go). This test is about git.
func TestConcurrentAcceptance_SerializesWithoutLoss(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	bootstrapIntegration(t, repoTop, integration)

	const n = 4
	type pending struct {
		id       string
		file     string
		proposal string
	}
	// Build the proposals sequentially: lane.Commit reads the shared working
	// tree, and racing the TREE is neither what submit does (each agent edits
	// its own files) nor what this test is about.
	prepared := make([]pending, 0, n)
	for i := range n {
		file := string(rune('p'+i)) + ".go"
		writeFile(t, repoTop, file, "package gate\n\nvar V = "+string(rune('0'+i))+"\n")
		id := NewCandidateID()
		prepared = append(prepared, pending{
			id: id, file: file,
			proposal: mustLaneCommit(t, laneOpts(repoTop, integration, "lane-"+id, file)),
		})
	}

	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range prepared {
		wg.Go(func() {
			p := prepared[i]
			env, err := Capture(context.Background(), CaptureParams{
				RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: p.proposal,
				Base: integration, WriteSet: []string{p.file}, CandidateID: p.id,
				Agent: domain.AgentUUID(tpAgentA), Session: domain.SessionUUID(tpAgentA + "-s"),
				BeadID: tcConcurBead,
			})
			if err != nil {
				errs[i] = err
				return
			}
			blob, err := env.WriteBlob(context.Background(), repoTop)
			if err != nil {
				errs[i] = err
				return
			}
			errs[i] = WriteCandidateRefs(context.Background(), repoTop, p.id, blob, p.proposal)
		})
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent acceptance %d failed: %v", i, err)
		}
	}

	// Every acceptance is visible, and whole.
	ids, err := ListCandidateIDs(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != n {
		t.Fatalf("%d candidate refs survived %d concurrent acceptances, want %d: %v", len(ids), n, n, ids)
	}
	for _, p := range prepared {
		if got := gitT(t, repoTop, "rev-parse", proposalRef(p.id)); got != p.proposal {
			t.Errorf("%s: proposal ref = %s, want %s", p.id, got, p.proposal)
		}
	}

	// And the resulting queue drains as one deterministic chain.
	rec := &claimRecorder{}
	q := promoteParams(repoTop, rec)
	q.MaxBatch = n
	res, err := Promote(context.Background(), q)
	if err != nil {
		t.Fatalf("Promote: %v", err)
	}
	for _, p := range prepared {
		if got := outcomeFor(t, res, p.id).Class; got != OutcomePromoted {
			t.Errorf("%s: class = %q, want promoted", p.id, got)
		}
	}
	tip := gitT(t, repoTop, "rev-parse", IntegrationRef)
	if got := len(strings.Split(gitT(t, repoTop, "rev-list", integration+".."+tip), "\n")); got != n {
		t.Errorf("integration advanced by %d commits, want %d", got, n)
	}
	for _, p := range prepared {
		if got := gitT(t, repoTop, "cat-file", "-t", tip+":"+p.file); got != tpBlob {
			t.Errorf("%s never reached integration (%q)", p.file, got)
		}
	}
}

// TestWriteCandidateRefs_ConcurrentSameIDHasExactlyOneWinner pins the other
// side of serialization: two acceptances that collide on one candidate id must
// resolve to one winner, not two half-applied transactions. "create" is a
// compare-and-swap against ref absence, and git's ref lock is what makes the
// loser lose cleanly.
func TestWriteCandidateRefs_ConcurrentSameIDHasExactlyOneWinner(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, tcPeerContent)
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	blob, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}

	results := make([]error, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			results[i] = WriteCandidateRefs(context.Background(), repoTop, "c1", blob, proposal)
		})
	}
	wg.Wait()

	winners := 0
	for _, err := range results {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("%d of 2 colliding acceptances succeeded, want exactly 1", winners)
	}
	if got := gitT(t, repoTop, "rev-parse", candidateRef("c1")); got != blob {
		t.Errorf("candidate ref = %s, want the envelope blob %s", got, blob)
	}
	if got := gitT(t, repoTop, "rev-parse", proposalRef("c1")); got != proposal {
		t.Errorf("proposal ref = %s, want the proposal commit %s", got, proposal)
	}
}
