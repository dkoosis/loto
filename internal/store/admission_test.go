package store

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/domain"
	"loto/internal/gate"
)

// gitT/writeTestFile mirror internal/gate's own test harness
// (envelope_test.go) — duplicated rather than exported, since exporting
// test-only helpers across packages for two call sites isn't worth the API
// surface.
const tcBead = "loto-ovno.4"

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=base", "GIT_AUTHOR_EMAIL=base@t",
		"GIT_COMMITTER_NAME=base", "GIT_COMMITTER_EMAIL=base@t",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeTestFile(t *testing.T, dir, rel, content string) {
	t.Helper()
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newGateRepo builds a throwaway git repo (independent of this file's own
// loto DB) for the git-plumbing half of AcceptCandidate — envelope capture and
// ref writes operate on a real repo, not the sqlite store.
func newGateRepo(t *testing.T) (repoTop, integration string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)
	t.Setenv("GIT_AUTHOR_DATE", "2026-01-01T00:00:00Z")
	t.Setenv("GIT_COMMITTER_DATE", "2026-01-01T00:00:00Z")

	repoTop = t.TempDir()
	gitT(t, repoTop, "init", "-q", "-b", "main")
	gitT(t, repoTop, "config", "commit.gpgsign", "false")
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 1\n")
	gitT(t, repoTop, "add", "-A")
	gitT(t, repoTop, "commit", "-qm", "base")
	return repoTop, gitT(t, repoTop, "rev-parse", "HEAD")
}

// buildProposal stages the working tree and commits it via plain plumbing
// (add + write-tree + commit-tree) — this file doesn't need lane.Commit's
// isolated-index machinery, just a real child commit of integration for
// gate.Capture to read.
func buildProposal(t *testing.T, repoTop, integration string) string {
	t.Helper()
	gitT(t, repoTop, "add", "-A")
	tree := gitT(t, repoTop, "write-tree")
	return gitT(t, repoTop, "commit-tree", tree, "-p", integration, "-m", "wip")
}

func TestAcceptCandidate_WritesRefsAndDurableClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)

	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "cand-1", BeadID: tcBead,
		LeaseEpoch: map[string]int64{tcAGo: 1},
	})
	if err != nil {
		t.Fatal(err)
	}

	submitter := domain.CandidateClaim{
		OwnerUUID: tcAlice, SessionUUID: tcAlice, Host: tcHost, PID: 1,
	}
	envSHA, err := s.AcceptCandidate(ctx, repoTop, env, submitter)
	if err != nil {
		t.Fatal(err)
	}
	if envSHA == "" {
		t.Fatal("AcceptCandidate returned an empty envelope SHA")
	}

	if got := gitT(t, repoTop, "rev-parse", "refs/loto/candidates/cand-1"); got != envSHA {
		t.Errorf("candidates ref = %s, want %s", got, envSHA)
	}
	if got := gitT(t, repoTop, "rev-parse", "refs/loto/proposals/cand-1"); got != proposal {
		t.Errorf("proposals ref = %s, want %s", got, proposal)
	}

	claims, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 || claims[0].PathCanonical != tcAGo || claims[0].CandidateID != "cand-1" {
		t.Fatalf("want one durable claim for a.go/cand-1, got %+v", claims)
	}
	if claims[0].OwnerUUID != tcAlice {
		t.Errorf("claim owner = %s, want %s", claims[0].OwnerUUID, tcAlice)
	}
}

// A candidate touching several paths must mint a claim for every one of them
// — the whole point of the durable claim is protecting every path the
// candidate is under review for, not just the first.
func TestAcceptCandidate_MintsOneClaimPerWriteSetPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcBGo, "package x\n\nvar B = 1\n")
	proposal := buildProposal(t, repoTop, integration)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcBGo}, CandidateID: "cand-2", BeadID: tcBead,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}); err != nil {
		t.Fatal(err)
	}
	claims, err := s.CandidateClaimsForPaths(ctx, []string{tcBGo})
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("want 1 claim for b.go, got %+v", claims)
	}
}

// Re-accepting the SAME candidate id must fail loudly (WriteCandidateRefs uses
// "create") rather than silently overwrite whichever candidate got there
// first — the double-admission guard.
func TestAcceptCandidate_RejectsDuplicateCandidateID(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	repoTop, integration := newGateRepo(t)
	writeTestFile(t, repoTop, tcAGo, "package x\n\nvar A = 2\n")
	proposal := buildProposal(t, repoTop, integration)

	env, err := gate.Capture(ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integration, ProposalSHA: proposal,
		Base: integration, WriteSet: []string{tcAGo}, CandidateID: "dup", BeadID: tcBead,
	})
	if err != nil {
		t.Fatal(err)
	}
	submitter := domain.CandidateClaim{OwnerUUID: tcAlice, Host: tcHost}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, submitter); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptCandidate(ctx, repoTop, env, submitter); err == nil {
		t.Fatal("want an error re-accepting the same candidate id, got nil")
	}
}

func TestRecordGateBypass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.RecordGateBypass(ctx, tcAlice, "gate binary missing"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM events WHERE event_kind = 'gate_bypass' AND actor_uuid = ?`, tcAlice,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("want 1 gate_bypass event, got %d", n)
	}
}

func TestMigrate_EventsCheckAllowsGateBypass(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	var ddl string
	if err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "'gate_bypass'") {
		t.Errorf("events CHECK constraint must allow 'gate_bypass', got: %s", ddl)
	}
}
