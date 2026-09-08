package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// twoDistinctProjects sets up two independently-slugged git repos under one
// shared HOME. LOTO_BASE is deliberately left UNSET, unlike withTempProject:
// loto-72i's whole point is per-project STATE separation, which the
// LOTO_BASE override (used for convenience by every other test in this
// package) collapses away — every project would land on one shared store
// regardless of repoTop, which is exactly the failure mode this test exists
// to rule out.
//
// Both repos are committed with refs/loto/integration pinned at HEAD, so
// either can be violations-scanned (mirrors cmd_violations_test.go's
// violationRepo, duplicated rather than reused because that helper is built
// on withTempProject's LOTO_BASE override).
func twoDistinctProjects(t *testing.T) (projA, projB string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("LOTO_BASE", "")
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	mk := func(name, remote string) string {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		initBareGitRepo(t, dir)
		addOriginRemote(t, dir, remote)
		if err := os.WriteFile(filepath.Join(dir, tcTargetA), []byte("original\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		submitGitT(t, dir, "add", "-A")
		submitGitT(t, dir, "commit", "-q", "-m", "base")
		submitGitT(t, dir, "update-ref", "refs/loto/integration", "HEAD")
		return dir
	}
	projA = mk("proj-a", "git@github.com:test/proj-a.git")
	projB = mk("proj-b", "git@github.com:test/proj-b.git")
	return projA, projB
}

// TestCmdBeacon_CrossProjectRoutesToOwningProject is ferret-72i's incident
// shape (AC "A test covers the incident shape: a caller in project A leases
// and then writes a path in project B, and project B's state reflects
// both"): a caller standing in project A beacons, then writes, a path that
// physically lives in project B. B's own store — not A's — must record the
// lease (AC1), A's store must carry no trace of it (Rule: "never silently
// leased against the caller's project"), and B's own violations scan must not
// flag the now-leased write (AC2: an unleased write is what violations
// reports — this one is leased).
func TestCmdBeacon_CrossProjectRoutesToOwningProject(t *testing.T) {
	projA, projB := twoDistinctProjects(t)
	targetRel := tcTargetA
	targetAbs := filepath.Join(projB, targetRel)

	t.Chdir(projA)
	pinAgent(t)

	var out, errBuf bytes.Buffer
	if code := Run([]string{gateIntentBeacon, targetAbs}, &out, &errBuf); code != 0 {
		t.Fatalf("cross-project beacon: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ beacon count=1") {
		t.Errorf("missing success row: %q", out.String())
	}

	// Rule: never silently leased against the caller's project — A's own
	// store carries no trace of it.
	var aOut, aErr bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &aOut, &aErr); code != 0 {
		t.Fatalf("status in A: exit=%d err=%q", code, aErr.String())
	}
	if strings.Contains(aOut.String(), targetRel) {
		t.Errorf("A's store recorded a lease that belongs to B: %q", aOut.String())
	}

	// AC1: B's own store DOES see it, read from a session standing in B.
	t.Chdir(projB)
	var bOut, bErr bytes.Buffer
	if code := Run([]string{"status"}, &bOut, &bErr); code != 0 {
		t.Fatalf("status in B: exit=%d err=%q", code, bErr.String())
	}
	if !strings.Contains(bOut.String(), targetRel) {
		t.Errorf("B's store did not record the lease: %q", bOut.String())
	}

	// The incident shape completes: the caller's write actually lands in B.
	if err := os.WriteFile(targetAbs, []byte("written\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// AC2/AC5: B's state reflects both the lease and the write — the write is
	// covered by B's own live lease, so B's own violations scan (run from B)
	// does not flag it as an unleased write.
	var vOut, vErr bytes.Buffer
	if code := Run([]string{"violations", "scan"}, &vOut, &vErr); code != 0 {
		t.Fatalf("violations scan in B flagged a leased write: exit=%d out=%q err=%q", code, vOut.String(), vErr.String())
	}
	if !strings.Contains(vOut.String(), "✓ violations count=0") {
		t.Errorf("expected a clean scan in B: %q", vOut.String())
	}
}

// TestCmdBeacon_PathOutsideEveryKnownProjectIsRefused is the Rule's negative
// case: a target that resolves into no git repository at all (never mind a
// DIFFERENT one) must be refused by name, not silently leased against the
// caller's own project.
func TestCmdBeacon_PathOutsideEveryKnownProjectIsRefused(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	outside := filepath.Join(filepath.Dir(repo), "not-a-repo", "file.go")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errBuf bytes.Buffer
	code := Run([]string{gateIntentBeacon, outside}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("expected refusal (exit 2), got exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), outside) {
		t.Errorf("refusal must name the path: %q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "no-owning-project") {
		t.Errorf("refusal must name the reason: %q", errBuf.String())
	}

	// It must not have been recorded against the caller's own project either.
	var sOut, sErr bytes.Buffer
	if code := Run([]string{"status"}, &sOut, &sErr); code != 0 {
		t.Fatalf("status: exit=%d err=%q", code, sErr.String())
	}
	if strings.Contains(sOut.String(), "file.go") {
		t.Errorf("path outside every project must not be leased against the caller's own: %q", sOut.String())
	}
}

// TestCmdBeacon_SameProjectUnchanged is the golden-diff AC: a beacon whose
// target lives in the caller's own project behaves exactly as before loto-72i
// — same success line, same single-store call shape (isCaller routes through
// openRuntime verbatim, not openRuntimeForRepoTop).
func TestCmdBeacon_SameProjectUnchanged(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)

	var out, errBuf bytes.Buffer
	if code := Run([]string{gateIntentBeacon, tcTargetA}, &out, &errBuf); code != 0 {
		t.Fatalf("same-project beacon: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ beacon count=1") {
		t.Errorf("missing success row: %q", out.String())
	}

	// An absolute spelling of the same in-repo path must behave identically.
	var out2, errBuf2 bytes.Buffer
	if code := Run([]string{gateIntentBeacon, filepath.Join(repo, tcTargetB)}, &out2, &errBuf2); code != 0 {
		t.Fatalf("same-project absolute beacon: exit=%d out=%q err=%q", code, out2.String(), errBuf2.String())
	}
	if !strings.Contains(out2.String(), "✓ beacon count=1") {
		t.Errorf("missing success row: %q", out2.String())
	}
}
