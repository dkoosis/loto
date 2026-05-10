package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestOnTimeoutAcquire_AllModes covers the three policy modes for
// `loto acquire --wait --on-timeout`. Same fixture (held path, 200ms wait)
// drives all three so the only varying axis is the policy decision.
func TestOnTimeoutAcquire_AllModes(t *testing.T) {
	cases := []struct {
		mode     string
		wantExit int
		wantOut  string // substring expected on stderr
	}{
		{policyBlock, 3, "suggested-action:abort"},
		{policyWarn, 0, "suggested-action:proceed"},
		{policySwitch, 1, "suggested-action:msg-and-switch"},
		{"", 3, "suggested-action:abort"}, // default == block
	}
	for _, tc := range cases {
		t.Run("mode="+tc.mode, func(t *testing.T) {
			base := t.TempDir()
			target := filepath.Join(t.TempDir(), "ot.go")
			if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if out, err := lotoCmd(base, flagAgentLong, "alpha", "acquire", target).Output(); err != nil {
				t.Fatalf("alpha acquire: %v\n%s", err, out)
			}

			args := []string{flagAgentLong, "beta", "--format", "llm", "acquire", "--wait", "200ms"}
			if tc.mode != "" {
				args = append(args, "--on-timeout", tc.mode)
			}
			args = append(args, target)
			cmd := lotoCmd(base, args...)
			out, err := cmd.Output()

			gotExit := 0
			if err != nil {
				var ee *exec.ExitError
				if !errors.As(err, &ee) {
					t.Fatalf("unexpected error: %v", err)
				}
				gotExit = ee.ExitCode()
				if !strings.Contains(string(ee.Stderr), tc.wantOut) {
					t.Errorf("stderr missing %q; got:\n%s", tc.wantOut, ee.Stderr)
				}
			} else {
				// warn mode exits 0 but still emits the timeout line on stderr.
				// Output() only captures stdout; rerun with CombinedOutput to see stderr.
				combined, _ := lotoCmd(base, args...).CombinedOutput()
				if !strings.Contains(string(combined), tc.wantOut) {
					t.Errorf("combined output missing %q; got:\n%s", tc.wantOut, combined)
				}
				_ = out
			}
			if gotExit != tc.wantExit {
				t.Errorf("exit: got %d want %d", gotExit, tc.wantExit)
			}
		})
	}
}

// TestOnTimeoutTryFile_Switch covers `loto try file --wait --on-timeout switch`
// — the primary skill use case. Default behavior of try (without --on-timeout)
// historically exited 1 with a blocked report; default block now exits 3.
func TestOnTimeoutTryFile_Switch(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "ts.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := lotoCmd(base, flagAgentLong, "alpha", "acquire", target).Output(); err != nil {
		t.Fatalf("alpha acquire: %v\n%s", err, out)
	}

	cmd := lotoCmd(base, flagAgentLong, "beta", "--format", "llm",
		"try", "file", "--wait", "200ms", "--on-timeout", "switch", target)
	out, err := cmd.Output()
	if err == nil {
		t.Fatalf("expected non-zero exit; got success: %s", out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("unexpected error type: %v", err)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit: got %d want 1", ee.ExitCode())
	}
	if !strings.Contains(string(ee.Stderr), "✗ timeout") {
		t.Errorf("stderr missing ✗ timeout marker; got:\n%s", ee.Stderr)
	}
	if !strings.Contains(string(ee.Stderr), "suggested-action:msg-and-switch") {
		t.Errorf("stderr missing suggested-action:msg-and-switch; got:\n%s", ee.Stderr)
	}
}

// TestOnTimeoutInvalidValue verifies typo-rejection at the flag boundary.
// Catches mistakes at invocation rather than letting them silently fall
// through to the default policy.
func TestOnTimeoutInvalidValue(t *testing.T) {
	base := t.TempDir()
	cmd := lotoCmd(base, flagAgentLong, "alpha", "acquire",
		"--wait", "100ms", "--on-timeout", "bogus", "/tmp/x")
	_, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	if ee.ExitCode() != 2 {
		t.Errorf("exit: got %d want 2", ee.ExitCode())
	}
	if !strings.Contains(string(ee.Stderr), "invalid --on-timeout") {
		t.Errorf("stderr missing usage error; got:\n%s", ee.Stderr)
	}
}

// TestOnTimeoutNoOpWithoutWait verifies --on-timeout is a no-op when --wait
// is absent (non-blocking try has no timeout to report).
func TestOnTimeoutNoOpWithoutWait(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "noop.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := lotoCmd(base, flagAgentLong, "alpha", "acquire", target).Output(); err != nil {
		t.Fatalf("alpha acquire: %v\n%s", err, out)
	}
	// No --wait; --on-timeout switch is silently irrelevant. Should still exit 1
	// with the standard blocked report (not a timeout report).
	cmd := lotoCmd(base, flagAgentLong, "beta", "--format", "llm",
		"try", "file", "--on-timeout", "switch", target)
	_, err := cmd.Output()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected ExitError, got %v", err)
	}
	if ee.ExitCode() != 1 {
		t.Errorf("exit: got %d want 1 (blocked, not timeout)", ee.ExitCode())
	}
	if strings.Contains(string(ee.Stderr), "✗ timeout") {
		t.Errorf("non-wait try should NOT emit ✗ timeout; got:\n%s", ee.Stderr)
	}
	if !strings.Contains(string(ee.Stderr), "✗ blocked") {
		t.Errorf("non-wait try should emit ✗ blocked; got:\n%s", ee.Stderr)
	}
}
