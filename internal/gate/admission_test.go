package gate

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// admitParamsFor builds the AdmitParams baseline for env: CurrentEpoch
// defaults to whatever the capture itself recorded for every write-set path
// — the "nothing has changed since capture" starting point every rejection
// test perturbs exactly one way.
func admitParamsFor(repoTop string, env Envelope, integration string) AdmitParams {
	epochs := make(map[string]int64, len(env.WriteSet))
	for _, p := range env.WriteSet {
		epochs[p] = env.LeaseEpoch[p]
	}
	return AdmitParams{
		RepoTop:              repoTop,
		PresentedProposalSHA: env.ProposalSHA,
		IntegrationRef:       integration,
		CurrentEpoch:         epochs,
	}
}

// tfCandidatesRefC1 is the refs/loto/candidates/<id> path the refs-transaction
// tests exercise repeatedly against a fixed id "c1".
const tfCandidatesRefC1 = "refs/loto/candidates/c1"

func mustAdmit(t *testing.T, env Envelope, p AdmitParams) Decision {
	t.Helper()
	d, err := Admit(context.Background(), env, p)
	if err != nil {
		t.Fatalf("Admit: %v", err)
	}
	return d
}

// TestAdmit_AcceptsCleanCandidate is the happy path every rejection test
// perturbs one field of: nothing has changed between capture and admission, so
// every check passes.
func TestAdmit_AcceptsCleanCandidate(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
		LeaseEpoch: map[string]int64{tfFileA: 1},
	})

	d := mustAdmit(t, env, admitParamsFor(repoTop, env, integration))
	if !d.Accepted {
		t.Fatalf("want accepted, got reject reason=%s detail=%s", d.Reason, d.Detail)
	}
}

func TestAdmit_RejectsProposalSHAMismatch(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	p := admitParamsFor(repoTop, env, integration)
	p.PresentedProposalSHA = integration // wrong commit presented
	d := mustAdmit(t, env, p)
	if d.Accepted || d.Reason != ReasonProposalSHAMismatch {
		t.Fatalf("want ReasonProposalSHAMismatch, got %+v", d)
	}
}

// The commit's REAL diff touches two paths; the envelope declares only one.
// checkDiffMatchesWriteSet re-derives the diff independently rather than
// trusting env.WriteSet, so a narrower declaration must be caught.
func TestAdmit_RejectsUnauthorizedPath_ExtraInDiff(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	writeFile(t, repoTop, tfFileB, "package gate\n\nvar B = 1\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA, tfFileB))
	// Capture claims a narrower write-set than the commit actually touches.
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	d := mustAdmit(t, env, admitParamsFor(repoTop, env, integration))
	if d.Accepted || d.Reason != ReasonUnauthorizedPath {
		t.Fatalf("want ReasonUnauthorizedPath (diff wider than write-set), got %+v", d)
	}
}

func TestAdmit_RejectsUnauthorizedPath_DeclaredButUntouched(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	env.WriteSet = append(env.WriteSet, tfFileB) // declares a path the diff never touched

	d := mustAdmit(t, env, admitParamsFor(repoTop, env, integration))
	if d.Accepted || d.Reason != ReasonUnauthorizedPath {
		t.Fatalf("want ReasonUnauthorizedPath (write-set wider than diff), got %+v", d)
	}
}

func TestAdmit_RejectsStaleLeaseEpoch(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
		LeaseEpoch: map[string]int64{tfFileA: 1},
	})

	p := admitParamsFor(repoTop, env, integration)
	p.CurrentEpoch[tfFileA] = 2 // the lease was released+regranted since capture
	d := mustAdmit(t, env, p)
	if d.Accepted || d.Reason != ReasonStaleLeaseEpoch {
		t.Fatalf("want ReasonStaleLeaseEpoch, got %+v", d)
	}
}

// A peer promoting a conflicting change to the SAME path between capture and
// admission must be caught — the preimage this candidate captured no longer
// matches integration.
func TestAdmit_RejectsStalePreimage(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	// Simulate a peer's promotion: integration moves past what env captured.
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 999 // promoted by someone else\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "peer promotion")
	newIntegration := gitT(t, repoTop, "rev-parse", "HEAD")

	p := admitParamsFor(repoTop, env, newIntegration)
	d := mustAdmit(t, env, p)
	if d.Accepted || d.Reason != ReasonStalePreimage {
		t.Fatalf("want ReasonStalePreimage, got %+v", d)
	}
}

// The "pkg/ moved under you" case: a created path's ancestor directory is
// re-checked at admission, not just capture.
func TestAdmit_RejectsStaleAncestry(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	writeFile(t, repoTop, "a/b/sibling.go", "package b\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "seed a/b")
	integration := gitT(t, repoTop, "rev-parse", "HEAD")

	writeFile(t, repoTop, "a/b/c.go", "package b\n\nvar C = 1\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", "a/b/c.go"))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{"a/b/c.go"}, CandidateID: "c1", BeadID: tfBead,
	})

	// A peer removes a/b/sibling.go AND replaces the whole a/b tree — simplest
	// is to delete a/ entirely by replacing it with a file of the same name.
	if err := os.RemoveAll(filepath.Join(repoTop, "a")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, repoTop, "a", "now a file, not a directory\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "a/ replaced by a file")
	newIntegration := gitT(t, repoTop, "rev-parse", "HEAD")

	p := admitParamsFor(repoTop, env, newIntegration)
	d := mustAdmit(t, env, p)
	if d.Accepted || d.Reason != ReasonStaleAncestry {
		t.Fatalf("want ReasonStaleAncestry, got %+v", d)
	}
}

func TestAdmit_RejectsMergeCommitShape(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	// Build a genuine two-parent merge commit via plumbing, matching the shape
	// a lane commit must NOT have.
	writeFile(t, repoTop, tfFileB, "package gate\n\nvar B = 1\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "second parent commit")
	otherParent := gitT(t, repoTop, "rev-parse", "HEAD")
	tree := gitT(t, repoTop, "rev-parse", "HEAD^{tree}")
	merge := gitT(t, repoTop, "commit-tree", tree, "-p", integration, "-p", otherParent, "-m", "merge")

	env := Envelope{
		CandidateID: "c1", ProposalSHA: merge, Base: integration,
		BeadID: tfBead, WriteSet: []string{tfFileB},
	}
	p := AdmitParams{RepoTop: repoTop, PresentedProposalSHA: merge, IntegrationRef: integration}
	d := mustAdmit(t, env, p)
	if d.Accepted || d.Reason != ReasonMalformedCandidate {
		t.Fatalf("want ReasonMalformedCandidate for a merge commit, got %+v", d)
	}
}

// --- refs transaction --------------------------------------------------

func TestUpdateRefsTx_WritesBothRefsAtomically(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	err := UpdateRefsTx(context.Background(), repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: tfCandidatesRefC1, NewSHA: integration},
		{Verb: VerbCreate, Ref: "refs/loto/proposals/c1", NewSHA: integration},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := gitT(t, repoTop, "rev-parse", tfCandidatesRefC1)
	if got != integration {
		t.Errorf("candidates ref = %s, want %s", got, integration)
	}
	got = gitT(t, repoTop, "rev-parse", "refs/loto/proposals/c1")
	if got != integration {
		t.Errorf("proposals ref = %s, want %s", got, integration)
	}
}

func TestUpdateRefsTx_CreateFailsOnExistingRef(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	if err := UpdateRefsTx(context.Background(), repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: tfCandidatesRefC1, NewSHA: integration},
	}); err != nil {
		t.Fatal(err)
	}
	err := UpdateRefsTx(context.Background(), repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: tfCandidatesRefC1, NewSHA: integration},
	})
	if err == nil {
		t.Fatal("want an error re-creating an existing ref (double-admission guard), got nil")
	}
}

// The atomicity claim itself: if one update in the transaction is invalid
// (creating a ref that already exists), NEITHER ref lands — not a partial
// write of just the valid one.
func TestUpdateRefsTx_PartialFailureLeavesNoRefs(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	if err := UpdateRefsTx(context.Background(), repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: tfCandidatesRefC1, NewSHA: integration},
	}); err != nil {
		t.Fatal(err)
	}

	err := UpdateRefsTx(context.Background(), repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: "refs/loto/candidates/c2", NewSHA: integration}, // fresh, would succeed alone
		{Verb: VerbCreate, Ref: tfCandidatesRefC1, NewSHA: integration},         // collides
	})
	if err == nil {
		t.Fatal("want an error from the colliding update")
	}
	if out := gitT(t, repoTop, "for-each-ref", "refs/loto/candidates/c2"); out != "" {
		t.Errorf("c2 must not exist — the transaction must not partially apply, got %q", out)
	}
}

func TestUpdateRefsTx_EmptyIsRejected(t *testing.T) {
	if err := UpdateRefsTx(context.Background(), t.TempDir(), nil); err == nil {
		t.Error("empty transaction must be rejected, not silently no-op")
	}
}

func TestWriteCandidateRefs(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	envSHA, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}

	if err := WriteCandidateRefs(context.Background(), repoTop, "c1", envSHA, proposal); err != nil {
		t.Fatal(err)
	}
	if got := gitT(t, repoTop, "rev-parse", tfCandidatesRefC1); got != envSHA {
		t.Errorf("candidates ref = %s, want envelope blob %s", got, envSHA)
	}
	if got := gitT(t, repoTop, "rev-parse", "refs/loto/proposals/c1"); got != proposal {
		t.Errorf("proposals ref = %s, want proposal commit %s", got, proposal)
	}
	// Round-trip: reading the ref back gives the same envelope Capture produced.
	back, err := ReadBlob(context.Background(), repoTop, envSHA)
	if err != nil {
		t.Fatal(err)
	}
	if back.ProposalSHA != env.ProposalSHA || back.CandidateID != env.CandidateID {
		t.Errorf("round-tripped envelope differs: got %+v, want %+v", back, env)
	}
}

// --- bypass --------------------------------------------------------------

func TestBypassed(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"off", true},
		{"", false},
		{"1", false},
		{"true", false},
		{"ON", false},
	}
	for _, c := range cases {
		t.Run("LOTO_GATE="+c.val, func(t *testing.T) {
			t.Setenv("LOTO_GATE", c.val)
			if got := Bypassed(); got != c.want {
				t.Errorf("Bypassed() = %v, want %v", got, c.want)
			}
		})
	}
}
