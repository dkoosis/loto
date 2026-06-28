package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestVerify_PassingCommand wraps lane.Verify: a zero-exit command against a
// real commit reports passed and exits 0.
func TestVerify_PassingCommand(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{"verify", "HEAD", "--", "sh", "-c", "exit 0"}, &out, &errB)
	if code != 0 {
		t.Fatalf("want exit 0, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✓ verify passed ") {
		t.Errorf("missing pass triage line: %q", out.String())
	}
}

// TestVerify_FailingCommand maps a non-zero command exit to a test failure
// (exit 1), not an infra error.
func TestVerify_FailingCommand(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{"verify", "HEAD", "--", "sh", "-c", "exit 3"}, &out, &errB)
	if code != 1 {
		t.Fatalf("want exit 1, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
	if !strings.HasPrefix(out.String(), "✗ verify failed ") {
		t.Errorf("missing fail triage line: %q", out.String())
	}
}

// TestVerify_SurfacesCommandOutput proves the wrapped command's stdout reaches
// the caller.
func TestVerify_SurfacesCommandOutput(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{"verify", "HEAD", "--", "sh", "-c", "echo HELLO_FROM_CMD"}, &out, &errB)
	if code != 0 {
		t.Fatalf("want exit 0, got %d; err=%q", code, errB.String())
	}
	if !strings.Contains(out.String(), "HELLO_FROM_CMD") {
		t.Errorf("verify did not surface command output: %q", out.String())
	}
}

// TestVerify_NoCommand_UsageError rejects an invocation with a commit but no
// command to run.
func TestVerify_NoCommand_UsageError(t *testing.T) {
	repo := withTempProject(t)
	commitAllInRepo(t, repo, "init")
	var out, errB bytes.Buffer
	code := Run([]string{"verify", "HEAD"}, &out, &errB)
	if code != 2 {
		t.Fatalf("want exit 2, got %d; out=%q err=%q", code, out.String(), errB.String())
	}
}
