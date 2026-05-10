//go:build unix

package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// hookJSON returns the CC tool-input JSON for a given file path.
func hookJSON(path string) string {
	type payload struct {
		ToolInput struct {
			FilePath string `json:"file_path"`
		} `json:"tool_input"`
	}
	var p payload
	p.ToolInput.FilePath = path
	b, _ := json.Marshal(p)
	return string(b)
}

// hookExecCmd builds an exec.Cmd for `loto hook <sub>` with stdin set to payload.
// Does NOT force --json so stderr output uses loto:llm:v1 format.
func hookExecCmd(base, sub, stdinPayload string, extraEnv ...string) *exec.Cmd {
	cmd := exec.Command(lotoBin, "--base", base, "hook", sub)
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	cmd.Stdin = strings.NewReader(stdinPayload)
	return cmd
}

// TestHookPreWrite_Uncontended: free path → exit 0, lock is then held.
func TestHookPreWrite_Uncontended(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "uncontended.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := hookExecCmd(base, "pre-write", hookJSON(target))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pre-write on free path: exit %v\n%s", err, out)
	}

	// Confirm the lock is now held by checking via loto acquire from another agent.
	out, err := lotoCmd(base, "--agent", "probe", "acquire", target).CombinedOutput()
	if err == nil {
		t.Fatalf("expected conflict after pre-write acquired path; got exit 0\n%s", out)
	}
}

// TestHookPreWrite_Contended: path held → exit 2 within LOTO_HOOK_WAIT, stderr names holder.
func TestHookPreWrite_Contended(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "contested.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Holder acquires path.
	if out, err := lotoCmd(base, "--agent", "holder", "acquire", target).Output(); err != nil {
		t.Fatalf("holder acquire: %v\n%s", err, out)
	}

	// pre-write with a short wait should exit 2 quickly.
	cmd := hookExecCmd(base, "pre-write", hookJSON(target), "LOTO_HOOK_WAIT=300ms")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("expected exit 2 on contended path; got exit 0\n%s", out)
	}
	var exitErr *exec.ExitError
	if ok := isExitError(err, &exitErr); !ok || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit 2; got %v\n%s", err, out)
	}
	stderr := string(out)
	if !strings.Contains(stderr, "loto:llm:v1") {
		t.Errorf("stderr missing loto:llm:v1 header\n%s", stderr)
	}
	if !strings.Contains(stderr, "holder") {
		t.Errorf("stderr missing holder agent name\n%s", stderr)
	}
	if !strings.Contains(stderr, "loto inbox") {
		t.Errorf("stderr missing loto inbox suggestion\n%s", stderr)
	}
}

// TestHookPostWrite_Releases: pre-write acquires, post-write releases; re-acquire succeeds.
func TestHookPostWrite_Releases(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "roundtrip.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out, err := hookExecCmd(base, "pre-write", hookJSON(target)).CombinedOutput(); err != nil {
		t.Fatalf("pre-write: %v\n%s", err, out)
	}
	if out, err := hookExecCmd(base, "post-write", hookJSON(target)).CombinedOutput(); err != nil {
		t.Fatalf("post-write: %v\n%s", err, out)
	}
	// Another agent can now acquire.
	if out, err := lotoCmd(base, "--agent", "new-owner", "acquire", target).Output(); err != nil {
		t.Fatalf("re-acquire after post-write: %v\n%s", err, out)
	}
}

// TestHookPostWrite_NoPriorAcquire: post-write with no prior acquire exits 0 (idempotent).
func TestHookPostWrite_NoPriorAcquire(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "never-held.go")

	cmd := hookExecCmd(base, "post-write", hookJSON(target))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("post-write with no prior acquire should exit 0: %v\n%s", err, out)
	}
}

// TestHookPreWrite_MissingFilePath: no file_path field → exit 0 silently.
func TestHookPreWrite_MissingFilePath(t *testing.T) {
	base := t.TempDir()
	cmd := hookExecCmd(base, "pre-write", `{"tool_input":{}}`)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("missing file_path should exit 0: %v\n%s", err, out)
	}
}

// TestHookPreWrite_BadJSON: malformed stdin → exit 0 with warning on stderr.
func TestHookPreWrite_BadJSON(t *testing.T) {
	base := t.TempDir()
	cmd := hookExecCmd(base, "pre-write", `not-json{`)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bad JSON should exit 0: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "JSON") && !strings.Contains(string(out), "json") {
		t.Errorf("expected stderr warning about bad JSON\n%s", out)
	}
}

// TestHookWait_EnvVar: LOTO_HOOK_WAIT configures the timeout duration.
// Uses a very short wait to confirm the env var is respected.
func TestHookWait_EnvVar(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "envwait.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := lotoCmd(base, "--agent", "blocker", "acquire", target).Output(); err != nil {
		t.Fatalf("blocker acquire: %v\n%s", err, out)
	}

	cmd := hookExecCmd(base, "pre-write", hookJSON(target), "LOTO_HOOK_WAIT=50ms")
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !isExitError(err, &exitErr) || exitErr.ExitCode() != 2 {
		t.Fatalf("expected exit 2 with short LOTO_HOOK_WAIT; got %v\n%s", err, out)
	}
}

func isExitError(err error, target **exec.ExitError) bool {
	if err == nil {
		return false
	}
	return errors.As(err, target)
}
