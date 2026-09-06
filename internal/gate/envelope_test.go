package gate

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/lane"
)

// Test-file path literals, hoisted per goconst — every scenario below reuses
// the same handful of paths in a fresh repo, so the repetition is real, not
// accidental.
const (
	tfFileA = "a.go"
	tfFileB = "b.go"
	tfFileZ = "z.go"
	tfBead  = "loto-ovno.3"
)

// --- harness ------------------------------------------------------------
// Mirrors internal/lane/lane_test.go's gitT/newBaseRepo shape so a reader who
// knows one package's tests recognizes the other's immediately.

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=base", "GIT_AUTHOR_EMAIL=base@t",
		"GIT_COMMITTER_NAME=base", "GIT_COMMITTER_EMAIL=base@t",
	)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, errb.String())
	}
	return strings.TrimSpace(out.String())
}

func writeFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newIntegrationRepo builds a repo with an "integration" branch carrying
// go.mod + a.go, and returns the working-tree root and integration's tip SHA.
// gate.Capture never reads the working tree or HEAD — everything is resolved
// against explicit tree-ish arguments — so the test never checks anything out.
func newIntegrationRepo(t *testing.T) (repoTop, integration string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")

	repoTop = t.TempDir()
	gitT(t, repoTop, "init", "-q", "-b", "main")
	gitT(t, repoTop, "config", "commit.gpgsign", "false")
	writeFile(t, repoTop, "go.mod", "module spike/gate\n\ngo 1.21\n")
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 1\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "base")
	return repoTop, gitT(t, repoTop, "rev-parse", "HEAD")
}

func mustLaneCommit(t *testing.T, opts lane.Opts) string {
	t.Helper()
	sha, err := lane.Commit(context.Background(), opts)
	if err != nil {
		t.Fatalf("lane.Commit: %v", err)
	}
	return sha
}

func laneOpts(repoTop, base, ref string, writeSet ...string) lane.Opts {
	return lane.Opts{
		RepoTop:   repoTop,
		Base:      base,
		Ref:       ref,
		WriteSet:  writeSet,
		Message:   "loto/" + ref + "\n\nCloses: none\n",
		Author:    lane.Identity{Name: "loto", Email: "loto@test"},
		Committer: lane.Identity{Name: "loto", Email: "loto@test"},
	}
}

func mustCapture(t *testing.T, p CaptureParams) Envelope {
	t.Helper()
	env, err := Capture(context.Background(), p)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	return env
}

// --- Capture: transitions -------------------------------------------------

func TestCapture_ModifiedPathRecordsBothSides(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	if len(env.Transitions) != 1 {
		t.Fatalf("want 1 transition, got %+v", env.Transitions)
	}
	tr := env.Transitions[0]
	if tr.Path != tfFileA {
		t.Errorf("path = %q, want a.go", tr.Path)
	}
	if tr.Expected == nil || tr.Result == nil {
		t.Fatalf("modified path must carry both sides, got %+v", tr)
	}
	if tr.Expected.SHA == tr.Result.SHA {
		t.Errorf("expected and result SHAs must differ after a real edit, both %q", tr.Expected.SHA)
	}
	if tr.Expected.Mode != "100644" || tr.Result.Mode != "100644" {
		t.Errorf("want regular-file mode on both sides, got expected=%s result=%s", tr.Expected.Mode, tr.Result.Mode)
	}
}

// TestCapture_DeletionRoundTripsThroughLaneCommit is the round-trip the bead
// explicitly calls out: lane.Commit's `git add -A` on a write-set path that no
// longer exists on disk must produce a tree where that path is truly absent,
// and Capture must read that as Result == nil, not as an error or a
// zero-value BlobRef.
func TestCapture_DeletionRoundTripsThroughLaneCommit(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	if err := os.Remove(filepath.Join(repoTop, tfFileA)); err != nil {
		t.Fatal(err)
	}
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	// Confirm at the git level, independent of gate, that the path is gone —
	// isolates a lane.Commit regression from a Capture bug if this ever fails.
	if out := gitT(t, repoTop, "ls-tree", proposal, "--", tfFileA); out != "" {
		t.Fatalf("precondition: a.go must be absent from the lane tree, got %q", out)
	}

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	tr := env.Transitions[0]
	if tr.Expected == nil {
		t.Fatal("a.go existed in integration; Expected must not be nil")
	}
	if tr.Result != nil {
		t.Errorf("deleted path must capture Result == nil, got %+v", tr.Result)
	}
}

func TestCapture_CreatedPathExpectedNil(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileB, "package gate\n\nvar B = 1\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileB))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileB}, CandidateID: "c1", BeadID: tfBead,
	})

	tr := env.Transitions[0]
	if tr.Expected != nil {
		t.Errorf("a brand-new path must capture Expected == nil, got %+v", tr.Expected)
	}
	if tr.Result == nil {
		t.Fatal("the created path must have a Result blob")
	}
}

// --- Capture: ancestry -----------------------------------------------------

// TestCapture_AncestryEntriesForCreatedPath pins the concrete example from
// git-gate.md: creating a/b/c.go records that a/ and a/b/ resolve to trees.
func TestCapture_AncestryEntriesForCreatedPath(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	// a/ and a/b/ must already exist in INTEGRATION for the ancestry check to
	// pass — an existing sibling file establishes both directories as trees.
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

	want := []string{"a", "a/b"}
	if len(env.Ancestry) != len(want) {
		t.Fatalf("ancestry = %+v, want entries for %v", env.Ancestry, want)
	}
	for i, w := range want {
		if env.Ancestry[i].Path != w {
			t.Errorf("ancestry[%d] = %q, want %q", i, env.Ancestry[i].Path, w)
		}
	}
}

// A modified (not created) path needs no ancestry entries: its parent already
// held a blob at capture time, which could not be true if the parent weren't
// a tree.
func TestCapture_ModifiedPathHasNoAncestryEntries(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	if len(env.Ancestry) != 0 {
		t.Errorf("modified top-level path must record no ancestry, got %+v", env.Ancestry)
	}
}

// TestCapture_AncestryDedupedAcrossWriteSet: two created paths sharing a
// parent must produce ONE ancestry entry for that parent, not two.
func TestCapture_AncestryDedupedAcrossWriteSet(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	writeFile(t, repoTop, "pkg/sibling.go", "package pkg\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "seed pkg")
	integration := gitT(t, repoTop, "rev-parse", "HEAD")

	writeFile(t, repoTop, "pkg/x.go", "package pkg\n\nvar X = 1\n")
	writeFile(t, repoTop, "pkg/y.go", "package pkg\n\nvar Y = 1\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", "pkg/x.go", "pkg/y.go"))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{"pkg/x.go", "pkg/y.go"}, CandidateID: "c1", BeadID: tfBead,
	})
	if len(env.Ancestry) != 1 || env.Ancestry[0].Path != "pkg" {
		t.Errorf("want one deduped ancestry entry for pkg, got %+v", env.Ancestry)
	}
}

// TestCapture_StaleAncestryRejected pins the "fail at the cheap end" case: a
// created path's declared parent does not resolve to a tree in integration
// (nothing has ever lived there), so the envelope must be refused rather than
// minted with a false precondition.
func TestCapture_StaleAncestryRejected(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, "ghost/new.go", "package ghost\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", "ghost/new.go"))

	_, err := Capture(context.Background(), CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{"ghost/new.go"}, CandidateID: "c1", BeadID: tfBead,
	})
	if !errors.Is(err, ErrAncestryNotTree) {
		t.Fatalf("want ErrAncestryNotTree, got %v", err)
	}
}

// --- Capture: lease epoch ---------------------------------------------------

func TestCapture_LeaseEpochThreadedPerPath(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
		LeaseEpoch: map[string]int64{tfFileA: 7},
	})
	if env.LeaseEpoch[tfFileA] != 7 {
		t.Errorf("LeaseEpoch[a.go] = %d, want 7", env.LeaseEpoch[tfFileA])
	}
}

// A path missing from the caller's LeaseEpoch map is not a Capture error — the
// missing-epoch judgment belongs to admission (a later bead), once the store's
// epoch column exists. Capture only threads through what it is given.
func TestCapture_MissingLeaseEpochDefaultsToZeroNotError(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))

	env, err := Capture(context.Background(), CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	if err != nil {
		t.Fatalf("Capture with no LeaseEpoch map must not error: %v", err)
	}
	if env.LeaseEpoch[tfFileA] != 0 {
		t.Errorf("want zero-value epoch, got %d", env.LeaseEpoch[tfFileA])
	}
}

// --- Capture: validation ----------------------------------------------------

func TestCapture_RejectsMissingRequiredFields(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	base := CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: integration,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	}
	cases := []struct {
		name string
		mut  func(p *CaptureParams)
	}{
		{"empty CandidateID", func(p *CaptureParams) { p.CandidateID = "" }},
		{"empty BeadID", func(p *CaptureParams) { p.BeadID = "" }},
		{"empty ProposalSHA", func(p *CaptureParams) { p.ProposalSHA = "" }},
		{"empty WriteSet", func(p *CaptureParams) { p.WriteSet = nil }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := base
			c.mut(&p)
			if _, err := Capture(context.Background(), p); err == nil {
				t.Fatalf("%s: want a validation error, got nil", c.name)
			}
		})
	}
}

func TestCapture_WriteSetSortedInEnvelope(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileZ, "package gate\n\nvar Z = 1\n")
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileZ, tfFileA))

	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileZ, tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})
	if env.WriteSet[0] != tfFileA || env.WriteSet[1] != tfFileZ {
		t.Errorf("WriteSet not sorted: %v", env.WriteSet)
	}
	if env.Transitions[0].Path != tfFileA || env.Transitions[1].Path != tfFileZ {
		t.Errorf("Transitions not sorted: %+v", env.Transitions)
	}
}

// --- Encode/Decode + object-store round-trip --------------------------------

func TestEnvelope_EncodeDecodeRoundTrip(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
		Agent: domain.AgentUUID("agent-1"), Session: domain.SessionUUID("sess-1"),
		LeaseEpoch: map[string]int64{tfFileA: 3},
	})

	data, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.CandidateID != env.CandidateID || got.BeadID != env.BeadID ||
		got.AgentUUID != env.AgentUUID || got.SessionUUID != env.SessionUUID {
		t.Errorf("identity fields did not round-trip: got %+v, want %+v", got, env)
	}
	if len(got.Transitions) != 1 || got.Transitions[0].Expected.SHA != env.Transitions[0].Expected.SHA {
		t.Errorf("transitions did not round-trip: got %+v, want %+v", got.Transitions, env.Transitions)
	}
	if got.LeaseEpoch[tfFileA] != 3 {
		t.Errorf("lease epoch did not round-trip: got %d", got.LeaseEpoch[tfFileA])
	}
}

// Same Envelope value must Encode to byte-identical output on every call —
// what makes hashing it into the object store meaningful (two captures of
// identical state converge on one blob, per design.md "same input →
// byte-identical output").
func TestEnvelope_EncodeIsDeterministic(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	a, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	b, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("Encode is not deterministic:\n%s\n---\n%s", a, b)
	}
}

func TestEnvelope_WriteReadBlobRoundTrip(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane1", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tfFileA}, CandidateID: "c1", BeadID: tfBead,
	})

	sha, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}
	if sha == "" {
		t.Fatal("WriteBlob returned an empty SHA")
	}
	got, err := ReadBlob(context.Background(), repoTop, sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.CandidateID != env.CandidateID || got.ProposalSHA != env.ProposalSHA {
		t.Errorf("round-tripped envelope differs: got %+v, want %+v", got, env)
	}

	// A blob written this way is content-addressed and unreachable by any ref
	// — writing the identical bytes twice must hash to the identical SHA.
	sha2, err := env.WriteBlob(context.Background(), repoTop)
	if err != nil {
		t.Fatal(err)
	}
	if sha2 != sha {
		t.Errorf("identical envelope content hashed to different SHAs: %s vs %s", sha, sha2)
	}
}

// --- blobAt / treeAt direct coverage ----------------------------------------

func TestGitRunner_BlobAtAbsentPathIsNilNotError(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	g := gitRunner{repoTop: repoTop}
	ref, err := g.blobAt(context.Background(), integration, "nope.go")
	if err != nil {
		t.Fatalf("absent path must not error: %v", err)
	}
	if ref != nil {
		t.Errorf("absent path must be nil, got %+v", ref)
	}
}

func TestGitRunner_BlobAtDirectoryIsRejected(t *testing.T) {
	repoTop, _ := newIntegrationRepo(t)
	writeFile(t, repoTop, "a/b.go", "package a\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "add dir")
	ref := gitT(t, repoTop, "rev-parse", "HEAD")

	g := gitRunner{repoTop: repoTop}
	_, err := g.blobAt(context.Background(), ref, "a")
	if !errors.Is(err, ErrPathNotBlob) {
		t.Fatalf("want ErrPathNotBlob for a directory path, got %v", err)
	}
}

// TestDecodeEnvelope_RejectsSemanticallyEmpty is the loto-amkj regression.
// `{}` is valid JSON, so it used to decode to a zero Envelope with a nil error.
// Promotion then found a candidate with no transitions, applied none, and
// advanced refs/loto/integration by an EMPTY, UNATTRIBUTED commit while
// retiring the candidate ref. git-gate.md's "a candidate ref without its
// envelope is rejected" has to mean semantically absent, not merely
// unparseable, or the guarantee only ever covered garbled bytes.
func TestDecodeEnvelope_RejectsSemanticallyEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		json string
	}{
		{"empty object", `{}`},
		{"no write set", `{"candidate_id":"c-1","proposal_sha":"abc","bead_id":"b","write_set":[],"transitions":[]}`},
		{"no candidate id", `{"proposal_sha":"abc","bead_id":"b","write_set":["a.go"],"transitions":[{"path":"a.go"}]}`},
		{"no proposal sha", `{"candidate_id":"c-1","bead_id":"b","write_set":["a.go"],"transitions":[{"path":"a.go"}]}`},
		{"no bead id", `{"candidate_id":"c-1","proposal_sha":"abc","write_set":["a.go"],"transitions":[{"path":"a.go"}]}`},
		{"transition count does not match write set", `{"candidate_id":"c-1","proposal_sha":"abc","bead_id":"b","write_set":["a.go","b.go"],"transitions":[{"path":"a.go"}]}`},
	} {
		if _, err := DecodeEnvelope([]byte(tc.json)); !errors.Is(err, ErrMalformedEnvelope) {
			t.Errorf("%s: err = %v, want ErrMalformedEnvelope", tc.name, err)
		}
	}
}

// TestDecodeEnvelope_AcceptsWhatCaptureMints pins the other side: the validator
// must not reject an envelope that legitimately came from Capture, or every
// candidate in flight becomes unreadable.
func TestDecodeEnvelope_AcceptsWhatCaptureMints(t *testing.T) {
	repoTop, integration := newIntegrationRepo(t)
	writeFile(t, repoTop, tfFileA, "package gate\n\nvar A = 2\n")
	proposal := mustLaneCommit(t, laneOpts(repoTop, integration, "lane-decode", tfFileA))
	env := mustCapture(t, CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal, Base: integration,
		WriteSet: []string{tfFileA}, CandidateID: "c-decode", BeadID: tfBead,
	})
	data, err := env.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodeEnvelope(data)
	if err != nil {
		t.Fatalf("a Capture-minted envelope must decode: %v", err)
	}
	if got.CandidateID != env.CandidateID || len(got.Transitions) != len(env.Transitions) {
		t.Errorf("round trip lost data: %+v", got)
	}
}

// --- git subprocess timeout (loto-4bde) -------------------------------------

// TestGitRunnerRunTimesOutOnHungGit pins the loto-4bde fix: gitRunner.run must
// not block for the full lifetime of a caller-supplied context when the git
// subprocess itself wedges (a stale index.lock, a hung fsmonitor). A fake
// "git" that never returns stands in for the hang; a short parent deadline
// (rather than waiting out the real gatePlumbingTimeout) keeps the test fast
// — context.WithTimeout inside run() picks whichever deadline is sooner.
//
// ‡ sleep 3, not 30: gitRunner.run does not override cmd.Cancel, so killing
// the fake git script (a /bin/sh process) sends SIGKILL only to that direct
// child — the `sleep` it invokes as its own child is not in the same kill
// path and is orphaned to run out its duration. A short sleep bounds that
// leak to a few seconds instead of leaving a 30s grandchild behind in CI.
func TestGitRunnerRunTimesOutOnHungGit(t *testing.T) {
	dir := t.TempDir()
	fakeGit := filepath.Join(dir, "git")
	if err := os.WriteFile(fakeGit, []byte("#!/bin/sh\nsleep 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	g := gitRunner{repoTop: t.TempDir()}
	start := time.Now()
	_, err := g.run(ctx, "status")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrGitTimeout) {
		t.Errorf("hung git err = %v, want ErrGitTimeout", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("run() took %v to return from a hung git subprocess, want well under gatePlumbingTimeout", elapsed)
	}
}

// TestGateGitEnv_PinsLCAllC pins the loto-in5f follow-up: every git
// subprocess this package launches must run under LC_ALL=C, last, so a
// LANG/LC_ALL the calling shell exports cannot translate git's stderr out
// from under refCASRejectionMarker's substring match. Asserted on the env
// slice directly (a white-box check of the mechanism) rather than on an
// actually-translated git message, which depends on locale catalogs the test
// box may not have installed.
func TestGateGitEnv_PinsLCAllC(t *testing.T) {
	t.Setenv("LANG", "fr_FR.UTF-8")
	t.Setenv("LC_ALL", "fr_FR.UTF-8")

	env := gateGitEnv()

	lastLCAll := ""
	for _, kv := range env {
		if strings.HasPrefix(kv, "LC_ALL=") {
			lastLCAll = kv
		}
	}
	if lastLCAll != "LC_ALL=C" {
		t.Errorf("last LC_ALL entry = %q, want LC_ALL=C to win the duplicate-key resolution regardless of the caller's own LC_ALL", lastLCAll)
	}

	// extra still wins over LC_ALL=C on ITS OWN key (e.g. GIT_INDEX_FILE),
	// since it is appended after.
	env = gateGitEnv("GIT_INDEX_FILE=/tmp/x")
	if env[len(env)-1] != "GIT_INDEX_FILE=/tmp/x" {
		t.Errorf("gateGitEnv with extra = %v, want the extra entry last", env)
	}
}
