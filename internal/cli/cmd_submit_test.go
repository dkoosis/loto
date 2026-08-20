package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/domain"
)

const (
	tcCmdSubmit   = "submit"
	tcIntentRaced = "raced"
	tcFlagBead    = "--bead"
	tcBeadOvno5   = "loto-ovno.5"
)

func submitGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestSubmit_HappyPath is the end-to-end vertical slice: lock, edit, submit —
// refs written, a durable candidate claim minted, and the original lock left
// untouched (AcceptCandidate's own documented posture, loto-ovno.4).
func TestSubmit_HappyPath(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("submit: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ candidate id=c-") {
		t.Errorf("missing success row: %q", out.String())
	}

	// Extract the candidate id and verify both refs landed.
	id := candidateIDFromSuccessRow(t, out.String())
	if sha := submitGitT(t, repo, "rev-parse", "--verify", "--quiet", "refs/loto/candidates/"+id); sha == "" {
		t.Error("refs/loto/candidates/<id> was not written")
	}
	if sha := submitGitT(t, repo, "rev-parse", "--verify", "--quiet", "refs/loto/proposals/"+id); sha == "" {
		t.Error("refs/loto/proposals/<id> was not written")
	}

	// The original lock must still be held — AcceptCandidate does not release it.
	statusOut := runOKSubmit(t, tcCmdStatus)
	if !strings.Contains(statusOut, tcTargetA) {
		t.Errorf("original lock must survive accept, status: %q", statusOut)
	}
}

// TestSubmit_RefusesWithoutLock: submitting a path the caller never locked
// must be refused at the lease-check step, before any git-gate machinery runs.
func TestSubmit_RefusesWithoutLock(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "no-lock-held") {
		t.Errorf("missing reason: %q", out.String())
	}
	// Nothing must have been written — no lane ref, no candidate ref.
	if sha := submitGitT(t, repo, "for-each-ref", "refs/loto/"); sha != "" {
		t.Errorf("a refused submit must write nothing under refs/loto/, got %q", sha)
	}
}

// TestSubmit_RefusesSharedLock: a shared lock is not the exclusive authority
// submit's model assumes — refused, same as lane's own precondition.
func TestSubmit_RefusesSharedLock(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest, tcFlagShared}, io.Discard, io.Discard); code != 0 {
		t.Fatal("shared lock")
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: out=%q", code, out.String())
	}
	if !strings.Contains(out.String(), "lock-not-exclusive") {
		t.Errorf("missing reason: %q", out.String())
	}
}

// TestSubmit_RendersAdmissionRejection exercises a REAL admission rejection
// through the full CLI flow, using submitAfterLeaseCheck to perturb the lock's
// epoch after the lease check passes but before Capture reads it — the exact
// TOCTOU window a stale-lease-epoch rejection exists to catch. No promotion
// machinery is needed to construct this: any store write that bumps the
// path's epoch between the two reads reproduces it.
func TestSubmit_RendersAdmissionRejection(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	a := pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	// The diff must actually match the write-set, or checkDiffMatchesWriteSet
	// rejects first (unauthorized-path) before the epoch check ever runs.
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { submitAfterLeaseCheck = nil })
	submitAfterLeaseCheck = func(rt *runtime) {
		// Release and re-acquire out from under the in-flight submit — a
		// genuine epoch bump (loto-ovno.2: release+reacquire increments).
		// Canonical targets are repo-relative (domain.Canonicalize), not
		// absolute — resolveCLITarget is the same resolution runSubmit itself
		// used to build `targets`, so this matches exactly.
		aTarget, err := resolveCLITarget(callerBase(), rt.RepoTop, tcTargetA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{aTarget}, domain.AgentUUID(a.UUID), rt.liveProbe()); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRaced}, io.Discard, io.Discard); code != 0 {
			t.Fatal("re-lock inside the hook")
		}
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want exit 1 (admission rejection), got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "stale-lease-epoch") {
		t.Errorf("missing reason: %q", out.String())
	}
	// A rejected submit must leave no candidate refs behind.
	if sha := submitGitT(t, repo, "for-each-ref", "refs/loto/candidates/"); sha != "" {
		t.Errorf("a rejected submit must write no candidate ref, got %q", sha)
	}
}

func TestSubmit_MissingBead_UsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("want exit 2, got %d: err=%q", code, errBuf.String())
	}
}

func TestSubmit_EmptyWriteSet_UsageError(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("want exit 2, got %d: err=%q", code, errBuf.String())
	}
}

// TestSubmit_RejectionLeavesNoLaneRef is Codex #259 P2: lane.Commit writes
// refs/heads/loto/<id> BEFORE the candidate has been judged, so a rejection
// used to leave that branch behind permanently — one per failed retry, against
// submit's own documented "on reject: nothing is written to git."
func TestSubmit_RejectionLeavesNoLaneRef(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	a := pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Same epoch-bump perturbation TestSubmit_RendersAdmissionRejection uses:
	// a real rejection, arrived at through the real flow.
	t.Cleanup(func() { submitAfterLeaseCheck = nil })
	submitAfterLeaseCheck = func(rt *runtime) {
		aTarget, err := resolveCLITarget(callerBase(), rt.RepoTop, tcTargetA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{aTarget}, domain.AgentUUID(a.UUID), rt.liveProbe()); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRaced}, io.Discard, io.Discard); code != 0 {
			t.Fatal("re-lock inside the hook")
		}
	}

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf); code != 1 {
		t.Fatalf("want exit 1 (rejection), got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}

	// No lane branch may survive a rejection, whatever the candidate id was.
	if refs := submitGitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/heads/loto/"); refs != "" {
		t.Errorf("rejected submit left lane refs behind: %q", refs)
	}
}

// TestSubmit_GateBypass is Codex #259 P1: LOTO_GATE=off is the documented
// outage escape hatch, and submit is its only caller — unchecked, the hatch is
// inert. The bypass must accept without admission, print the loud advisory,
// and leave the mandatory audit event behind.
func TestSubmit_GateBypass(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)
	t.Setenv("LOTO_GATE", "off")

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("bypass submit: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "LOTO_GATE=off") {
		t.Errorf("bypass must print the advisory: %q", errBuf.String())
	}
	id := candidateIDFromSuccessRow(t, out.String())
	if sha := submitGitT(t, repo, "rev-parse", "--verify", "--quiet", "refs/loto/candidates/"+id); sha == "" {
		t.Error("bypass must still write the candidate ref")
	}
}

// candidateIDFromSuccessRow pulls "id=c-..." out of the success row.
func candidateIDFromSuccessRow(t *testing.T, out string) string {
	t.Helper()
	for f := range strings.FieldsSeq(out) {
		if v, ok := strings.CutPrefix(f, "id="); ok {
			return v
		}
	}
	t.Fatalf("no id= in %q", out)
	return ""
}

func runOKSubmit(t *testing.T, argv ...string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run(argv, &out, &errBuf); code != 0 {
		t.Fatalf("%v exit=%d out=%q err=%q", argv, code, out.String(), errBuf.String())
	}
	return out.String()
}

// TestSubmit_LeaseLostBetweenAdmissionAndClaim is loto-ovno.10 end-to-end:
// admission judges on an epoch map read before AcceptCandidate takes the
// op-flock, so a lease released and regranted in THAT window used to produce a
// live peer lock and a candidate claim on the same path. The submit must lose,
// and lose as a rejection — exit 1 with a reason, not an internal error.
func TestSubmit_LeaseLostBetweenAdmissionAndClaim(t *testing.T) {
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	a := pinAgent(t)

	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Perturb AFTER admission accepts — submitAfterLeaseCheck fires too early
	// to exercise this window, which is the whole point of the second seam.
	t.Cleanup(func() { submitBeforeAccept = nil })
	submitBeforeAccept = func(rt *runtime) {
		aTarget, err := resolveCLITarget(callerBase(), rt.RepoTop, tcTargetA)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rt.Store.ReleaseLocks(rt.Ctx, []domain.Target{aTarget}, domain.AgentUUID(a.UUID), rt.liveProbe()); err != nil {
			t.Fatal(err)
		}
		if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentRaced}, io.Discard, io.Discard); code != 0 {
			t.Fatal("re-lock inside the hook")
		}
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want exit 1 (rejection), got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "stale-lease-epoch") ||
		!strings.Contains(out.String(), "lease-epoch-changed") {
		t.Errorf("missing rejection detail: %q", out.String())
	}
	// Neither half of the corruption may survive: no claim beside the new
	// lease, and no refs for a candidate that never landed.
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Store.Close()
	claims, err := rt.Store.ListCandidateClaims(rt.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 0 {
		t.Errorf("a lost race must claim nothing, got %+v", claims)
	}
	if refs := submitGitT(t, repo, "for-each-ref", "--format=%(refname)",
		"refs/loto/candidates/", "refs/loto/proposals/"); refs != "" {
		t.Errorf("a lost race must write no candidate refs, got %q", refs)
	}
}
