//go:build unix

package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHookPreWrite_DeniesTerminal: human invocation (stdin is tty) → exit non-zero,
// stderr explains the surface mismatch. Forced via LOTO_SURFACE_TEST=tty.
func TestHookPreWrite_DeniesTerminal(t *testing.T) {
	base := t.TempDir()
	cmd := exec.Command(lotoBin, "--base", base, "hook", "pre-write")
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1", "LOTO_SURFACE_TEST=tty")
	// no stdin set → child's stdin is /dev/null, but the env var forces the check
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit on tty stdin; got success\n%s", out)
	}
	if !strings.Contains(string(out), "not for interactive use") {
		t.Fatalf("expected wrong-surface message; got:\n%s", out)
	}
}

// TestHookPostWrite_DeniesTerminal: same denial for post-write.
func TestHookPostWrite_DeniesTerminal(t *testing.T) {
	base := t.TempDir()
	cmd := exec.Command(lotoBin, "--base", base, "hook", "post-write")
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1", "LOTO_SURFACE_TEST=tty")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit on tty stdin; got success\n%s", out)
	}
	if !strings.Contains(string(out), "not for interactive use") {
		t.Fatalf("expected wrong-surface message; got:\n%s", out)
	}
}

// TestHookPreWrite_AllowsPipedStdin: piped stdin (default in tests) → the
// surface check is silent and the existing JSON-empty fail-safe path runs.
func TestHookPreWrite_AllowsPipedStdin(t *testing.T) {
	base := t.TempDir()
	cmd := exec.Command(lotoBin, "--base", base, "hook", "pre-write")
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1")
	cmd.Stdin = strings.NewReader("") // empty pipe → fail-safe exit 0
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("piped empty stdin should fail-safe; got err=%v\n%s", err, out)
	}
}

// TestDashboard_DeniesNonTerminalTUI: --format=json with piped stdout (test
// harness) tries to enter TUI; surface check denies it loudly.
func TestDashboard_DeniesNonTerminalTUI(t *testing.T) {
	base := t.TempDir()
	cmd := exec.Command(lotoBin, "--base", base, "--format", "json", "dashboard")
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected non-zero exit when TUI requested without a terminal; got success\n%s", out)
	}
	if !strings.Contains(string(out), "wrong-surface") {
		t.Fatalf("expected wrong-surface message; got:\n%s", out)
	}
}
