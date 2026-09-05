package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tcCmdPromote  = "promote"
	tcVerifyTrue  = "true"
	tcVerifyFalse = "false"
	// tcPromoteEmpty is the whole empty-status line, spelled out: an operator
	// with no candidates must still get every counter, not silence (design.md).
	tcPromoteEmpty = "ℹ promote candidates=0 promoted=0 stale=0 rejected=0 requeued=0 infra=0 verifies=0 integration=absent dry-run=true"
)

// promoteRepo is the shared fixture: a committed temp project with a pinned
// identity, ready to lock -> edit -> submit -> promote.
func promoteRepo(t *testing.T) string {
	t.Helper()
	repo := withTempProject(t)
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	pinAgent(t)
	return repo
}

// promoteSubmitOne locks tcTargetA, edits it, and submits — the state a
// promotion needs in front of it.
func promoteSubmitOne(t *testing.T, repo, content string) {
	t.Helper()
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock")
	}
	if err := os.WriteFile(filepath.Join(repo, tcTargetA), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdSubmit, tcTargetA, tcFlagBead, tcBeadOvno5}, &out, &errBuf); code != 0 {
		t.Fatalf("submit: exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
}

// TestPromote_ListedInRootHelp pins AC1's first half: the verb is registered
// and bare `loto` names it. A verb nobody can find is the state this bead
// exists to leave behind — gate.Promote had no caller but a test.
func TestPromote_ListedInRootHelp(t *testing.T) {
	stdout, stderr, code := executeCommand()
	if code != 2 {
		t.Fatalf("bare loto: want exit 2, got %d", code)
	}
	if !strings.Contains(stdout+stderr, "promote  Drain accepted candidates") {
		t.Fatalf("root help does not list promote:\n%s", stdout+stderr)
	}
	if _, ok := registry[tcCmdPromote]; !ok {
		t.Fatal("promote not registered")
	}
}

// TestPromote_DryRunEmptyStatus pins AC1's second half: zero candidates prints
// the explicit empty-status line and exits 0. It must also touch no ref —
// refs/loto/integration is created by a promotion, never by a report about one.
func TestPromote_DryRunEmptyStatus(t *testing.T) {
	repo := promoteRepo(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote, tcFlagDryRun}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want exit 0, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if got := strings.TrimSpace(out.String()); got != tcPromoteEmpty {
		t.Fatalf("empty-status line:\n got %q\nwant %q", got, tcPromoteEmpty)
	}
	// for-each-ref, not rev-parse: a missing ref is the expected state here and
	// rev-parse reports it by exiting 1, which the git helper treats as fatal.
	if refs := submitGitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/loto/integration"); refs != "" {
		t.Errorf("dry run created %s", refs)
	}
}

// TestPromote_DryRunListsCandidateAndPromotesNothing: a submitted candidate is
// reported, and integration does not move.
func TestPromote_DryRunListsCandidate(t *testing.T) {
	repo := promoteRepo(t)
	promoteSubmitOne(t, repo, "edited\n")
	before := submitGitT(t, repo, "rev-parse", "refs/loto/integration")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote, tcFlagDryRun}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want exit 0, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "promote candidates=1") {
		t.Errorf("triage line missing the candidate: %q", out.String())
	}
	if !strings.Contains(out.String(), "action=would-consider") {
		t.Errorf("missing candidate row: %q", out.String())
	}
	if after := submitGitT(t, repo, "rev-parse", "refs/loto/integration"); after != before {
		t.Errorf("dry run advanced integration %s -> %s", before, after)
	}
}

// TestPromote_PassesThroughLiteralLeadingDashDash pins loto-aq8x: a wrapped
// command whose OWN first token is a literal "--" survives intact through
// `loto promote -- -- --check` as ["--", "--check"], rather than the manual
// strip eating that leading "--" (thinking it was loto's own separator,
// already consumed by flag.Parse) and leaving gate.Promote to try running a
// nonexistent "--check" executable.
//
// The wrapped "command" is a real executable literally named "--": finding
// and running it at all proves cmd[0] survived as "--"; its own exit code
// proves cmd[1] arrived as exactly "--check" and nothing else.
func TestPromote_PassesThroughLiteralLeadingDashDash(t *testing.T) {
	repo := promoteRepo(t)
	promoteSubmitOne(t, repo, "edited\n")

	bin := t.TempDir()
	script := filepath.Join(bin, "--")
	body := "#!/bin/sh\n[ \"$#\" -eq 1 ] && [ \"$1\" = \"--check\" ] && exit 0\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote, "--", "--", "--check"}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want exit 0 (promoted), got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(out.String(), "action=promoted") {
		t.Errorf("candidate not promoted — wrapped command did not receive [\"--\", \"--check\"]: %q", out.String())
	}
}

// TestPromote_HappyPath is the end-to-end slice this bead unblocks: a
// candidate submitted through the CLI is landed on refs/loto/integration by
// the CLI, its refs retired, and the phase-2 verify reported with its own
// duration — the number the loto-ovno.1 budget is stated over.
func TestPromote_HappyPath(t *testing.T) {
	repo := promoteRepo(t)
	promoteSubmitOne(t, repo, "edited\n")
	before := submitGitT(t, repo, "rev-parse", "refs/loto/integration")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote, "--", tcVerifyTrue}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want exit 0, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "✓ promote candidates=1 promoted=1") {
		t.Errorf("triage line: %q", got)
	}
	if !strings.Contains(got, "action=promoted") || !strings.Contains(got, "bead="+tcBeadOvno5) {
		t.Errorf("missing promoted row: %q", got)
	}
	if !strings.Contains(got, "ℹ verify batch=b-") || !strings.Contains(got, "result=pass") {
		t.Errorf("missing verify timing row: %q", got)
	}
	// tree= rides the same row. Without it ms= cannot be read: a slow verify
	// looks identical whether the invariant command is heavy or the reused
	// worktree quietly stopped working.
	if !strings.Contains(got, " tree=") {
		t.Errorf("verify row does not say which tree ran: %q", got)
	}

	after := submitGitT(t, repo, "rev-parse", "refs/loto/integration")
	if after == before {
		t.Fatalf("integration did not advance from %s", before)
	}
	if body := submitGitT(t, repo, "show", "-s", "--format=%s", after); !strings.HasPrefix(body, "loto: promote ") {
		t.Errorf("integration tip is not a promotion commit: %q", body)
	}
	if content := submitGitT(t, repo, "show", after+":"+tcTargetA); content != "edited" {
		t.Errorf("promoted content = %q", content)
	}
	if refs := submitGitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/loto/candidates/"); refs != "" {
		t.Errorf("candidate refs survived promotion: %q", refs)
	}
}

// TestPromote_VerifyRedRejects: a candidate that fails verify alone is retired
// — exit 1, a ✗ row naming the class, and a runnable resubmit line.
func TestPromote_VerifyRedRejects(t *testing.T) {
	repo := promoteRepo(t)
	promoteSubmitOne(t, repo, "edited\n")
	before := submitGitT(t, repo, "rev-parse", "refs/loto/integration")

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote, "--", tcVerifyFalse}, &out, &errBuf)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	got := out.String()
	if !strings.Contains(got, "✗ promote candidates=1") || !strings.Contains(got, "rejected=1") {
		t.Errorf("triage line: %q", got)
	}
	if !strings.Contains(got, "action=verify-red") {
		t.Errorf("missing rejection row: %q", got)
	}
	if !strings.Contains(got, "loto submit <file>... --bead "+tcBeadOvno5) {
		t.Errorf("missing fix block: %q", got)
	}
	if after := submitGitT(t, repo, "rev-parse", "refs/loto/integration"); after != before {
		t.Errorf("a red candidate moved integration %s -> %s", before, after)
	}
	if refs := submitGitT(t, repo, "for-each-ref", "--format=%(refname)", "refs/loto/candidates/"); refs != "" {
		t.Errorf("rejected candidate refs survived: %q", refs)
	}
}

// TestPromote_RequiresVerifyCommand: a real run has no default invariant
// command to fall back on, so it refuses rather than promoting unverified.
func TestPromote_RequiresVerifyCommand(t *testing.T) {
	promoteRepo(t)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdPromote}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("want exit 2, got %d: out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "verify command required") {
		t.Errorf("missing reason: %q", errBuf.String())
	}
}

// TestPromote_RejectsNegativeBounds: --max-batch and --max-rounds take 0 for
// "the gate default"; a negative would silently mean the same thing inside
// gate.Promote, which reads as the flag having been ignored.
func TestPromote_RejectsNegativeBounds(t *testing.T) {
	promoteRepo(t)

	for _, flag := range []string{"--max-batch=-1", "--max-rounds=-2"} {
		var out, errBuf bytes.Buffer
		code := Run([]string{tcCmdPromote, flag, "--", tcVerifyTrue}, &out, &errBuf)
		if code != 2 {
			t.Errorf("%s: want exit 2, got %d: err=%q", flag, code, errBuf.String())
		}
	}
}
