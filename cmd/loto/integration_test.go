//go:build unix

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// lotoBin is set by TestMain to the path of the compiled loto binary.
var lotoBin string

func TestMain(m *testing.M) {
	bin, err := buildLotoBinary()
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration: build loto: %v\n", err)
		os.Exit(1)
	}
	lotoBin = bin
	os.Exit(m.Run())
}

// buildLotoBinary compiles the loto CLI into a temp directory.
func buildLotoBinary() (string, error) {
	dir, err := os.MkdirTemp("", "loto-integ-*")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "loto")
	// Build from repo root (two levels up from cmd/loto).
	root := filepath.Join("..", "..")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/loto")
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("build failed: %w\n%s", err, out)
	}
	return bin, nil
}

// lotoCmd returns an *exec.Cmd for the loto binary with a private base dir.
func lotoCmd(base string, args ...string) *exec.Cmd {
	full := append([]string{"--base", base, "--json"}, args...)
	cmd := exec.Command(lotoBin, full...)
	cmd.Env = append(os.Environ(), "LOTO_SUPPRESS_LEGACY_WARNING=1")
	return cmd
}

// TestContendedAcquire: a holder keeps the lock; N concurrent contenders all
// must fail (exit 1) with holder JSON identifying the winner.
func TestContendedAcquire(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "contested.go")

	// Start holder with --hold.
	holder := lotoCmd(base, "--agent", "holder", "try", "file", "--hold", target)
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal("start holder:", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })

	// Wait for holder to confirm acquisition.
	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(holderOut)
		for sc.Scan() {
			line := sc.Text()
			if strings.Contains(line, `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not confirm acquisition within 5s")
	}

	// Launch N contenders concurrently; all must fail with exit 1.
	const N = 5
	type result struct {
		code int
		body []byte
	}
	results := make([]result, N)
	var wg sync.WaitGroup
	for i := range N {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := lotoCmd(base, "--agent", fmt.Sprintf("contender-%d", idx),
				"try", "file", target)
			out, err := cmd.Output()
			code := 0
			if err != nil {
				var ee *exec.ExitError
				if errors.As(err, &ee) {
					code = ee.ExitCode()
					out = ee.Stderr
				}
			}
			results[idx] = result{code: code, body: out}
		}(i)
	}
	wg.Wait()

	for i, r := range results {
		if r.code != 1 {
			t.Errorf("contender-%d: expected exit 1, got %d", i, r.code)
		}
		// stderr JSON should identify the holder.
		if !strings.Contains(string(r.body), "holder") {
			t.Errorf("contender-%d: holder not named in output: %s", i, r.body)
		}
	}
}

// TestCrashRecovery: child acquires with --hold; parent SIGKILLs it; next
// TryFileLock must succeed and the dead agent's tag must be gone.
func TestCrashRecovery(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(t.TempDir(), "recover.go")

	// Start child with --hold.
	child := lotoCmd(base, "--agent", "doomed-agent", "try", "file", "--hold", target)
	childOut, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal("start child:", err)
	}

	// Wait for child to confirm acquisition.
	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(childOut)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("child did not confirm acquisition within 5s")
	}

	// SIGKILL: lock is held by a now-dead process.
	if err := child.Process.Signal(syscall.SIGKILL); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	_ = child.Wait()

	// Give the kernel a moment to release the flock.
	time.Sleep(50 * time.Millisecond)

	// New agent acquires the same target — must succeed (dead-PID GC).
	cmd := lotoCmd(base, "--agent", "survivor", "try", "file", target)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("survivor acquire failed: %v\n%s", err, out)
	}

	// Parse output and verify the tag belongs to survivor, not doomed-agent.
	var result map[string]any
	if jsonErr := json.Unmarshal(out, &result); jsonErr != nil {
		t.Fatalf("parse survivor output: %v\n%s", jsonErr, out)
	}
	if result["agent"] != "survivor" {
		t.Errorf("expected agent=survivor in output, got %v", result["agent"])
	}

	// The doomed-agent's tag should be gone — status should show survivor or free.
	statCmd := lotoCmd(base, "status", target)
	statOut, err := statCmd.Output()
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if strings.Contains(string(statOut), "doomed-agent") {
		t.Errorf("doomed-agent tag persists after crash recovery:\n%s", statOut)
	}
}

// initGitRepo creates a minimal git repo in dir (init + initial empty commit).
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// TestCheckPathsBlockedByLock: check-paths --staged exits 1 when a staged file
// is locked by a different agent.
func TestCheckPathsBlockedByLock(t *testing.T) {
	rawRepo := t.TempDir()
	// Resolve symlinks so filepath.Abs inside the subprocess matches the path
	// used when acquiring the lock (macOS: /var/folders → /private/var/folders).
	repoDir, err := filepath.EvalSymlinks(rawRepo)
	if err != nil {
		t.Fatal(err)
	}
	initGitRepo(t, repoDir)
	base := t.TempDir()

	// Create and stage a file.
	target := filepath.Join(repoDir, "work.go")
	if err := os.WriteFile(target, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", "work.go")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	// Acquire a lock on the file as a different agent using --hold.
	holder := lotoCmd(base, "--agent", "blocker", "try", "file", "--hold", target)
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := holder.Start(); err != nil {
		t.Fatal("start holder:", err)
	}
	t.Cleanup(func() { _ = holder.Process.Kill(); _ = holder.Wait() })

	acquired := make(chan struct{})
	go func() {
		sc := bufio.NewScanner(holderOut)
		for sc.Scan() {
			if strings.Contains(sc.Text(), `"acquired"`) {
				close(acquired)
				return
			}
		}
	}()
	select {
	case <-acquired:
	case <-time.After(5 * time.Second):
		t.Fatal("holder did not confirm acquisition within 5s")
	}

	// check-paths --staged as a different agent: should exit 1.
	checkCmd := lotoCmd(base, "--agent", "other-agent", "check-paths", "--staged")
	checkCmd.Dir = repoDir
	out, err := checkCmd.Output()
	if err == nil {
		t.Fatalf("expected check-paths to fail (exit 1), got success; output: %s", out)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("expected exit 1, got %v", err)
	}
}

// TestCheckPathsPassesWhenFree: check-paths --staged exits 0 when no locks conflict.
func TestCheckPathsPassesWhenFree(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)
	base := t.TempDir()

	target := filepath.Join(repoDir, "clean.go")
	if err := os.WriteFile(target, []byte("package main"), 0o600); err != nil {
		t.Fatal(err)
	}
	addCmd := exec.Command("git", "add", "clean.go")
	addCmd.Dir = repoDir
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}

	checkCmd := lotoCmd(base, "--agent", "my-agent", "check-paths", "--staged")
	checkCmd.Dir = repoDir
	if out, err := checkCmd.Output(); err != nil {
		t.Fatalf("check-paths expected success: %v\n%s", err, out)
	}
}

// TestInstallGitHookIdempotent: install-git-hook writes the hook; re-running
// does not duplicate the loto block.
func TestInstallGitHookIdempotent(t *testing.T) {
	repoDir := t.TempDir()
	initGitRepo(t, repoDir)
	base := t.TempDir()

	run := func() {
		t.Helper()
		cmd := lotoCmd(base, "install-git-hook")
		cmd.Dir = repoDir
		if out, err := cmd.Output(); err != nil {
			t.Fatalf("install-git-hook: %v\n%s", err, out)
		}
	}

	run()
	run() // idempotent

	hookContent, err := os.ReadFile(filepath.Join(repoDir, ".git", "hooks", "pre-commit"))
	if err != nil {
		t.Fatalf("read pre-commit hook: %v", err)
	}
	s := string(hookContent)
	if !strings.Contains(s, "loto check-paths --staged") {
		t.Errorf("hook missing loto check-paths line:\n%s", s)
	}
	// Should appear exactly once.
	if count := strings.Count(s, "loto check-paths --staged"); count != 1 {
		t.Errorf("expected loto check-paths to appear once, got %d:\n%s", count, s)
	}
}

// TestSessionIDStableHandle verifies the one-handle-per-session invariant:
// two independent loto whoami invocations with the same CLAUDE_SESSION_ID
// (and no LOTO_AGENT_ID) produce the same agent id and handle.
func TestSessionIDStableHandle(t *testing.T) {
	if lotoBin == "" {
		t.Skip("loto binary not built")
	}
	homeDir := t.TempDir()

	// Build a clean env: keep PATH + the system bits we need, drop any
	// inherited LOTO_AGENT_ID, point HOME at a fresh dir so ~/.loto is empty.
	cleanEnv := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + homeDir,
		"CLAUDE_SESSION_ID=test-session-xyz",
		"LOTO_SUPPRESS_LEGACY_WARNING=1",
	}

	run := func() map[string]any {
		t.Helper()
		cmd := exec.Command(lotoBin, "whoami", "--json")
		cmd.Env = cleanEnv
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("whoami: %v\n%s", err, out)
		}
		var got map[string]any
		if err := json.Unmarshal(out, &got); err != nil {
			t.Fatalf("parse: %v\n%s", err, out)
		}
		return got
	}

	first := run()
	second := run()

	if first["id"] != second["id"] {
		t.Errorf("id drift across calls: %v != %v", first["id"], second["id"])
	}
	if first["handle"] != second["handle"] {
		t.Errorf("handle drift across calls: %v != %v", first["handle"], second["handle"])
	}

	// Exactly one agent file should exist for this session.
	agentDir := filepath.Join(homeDir, ".loto", "agents")
	entries, err := os.ReadDir(agentDir)
	if err != nil {
		t.Fatalf("read agent dir: %v", err)
	}
	if len(entries) != 1 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("expected 1 agent file, got %d: %v", len(entries), names)
	}
}

// TestIntegrationWhoamiFormatDefaults verifies that piped stdout defaults to
// the LLM format and --json continues to emit valid JSON for back-compat.
func TestIntegrationWhoamiFormatDefaults(t *testing.T) {
	if lotoBin == "" {
		t.Skip("loto binary not built")
	}
	out, err := exec.Command(lotoBin, "whoami").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "loto:llm:v1\n") {
		t.Fatalf("expected llm header on piped stdout, got:\n%s", out)
	}
	out2, err := exec.Command(lotoBin, "whoami", "--json").CombinedOutput()
	if err != nil {
		t.Fatalf("whoami --json: %v\n%s", err, out2)
	}
	var got map[string]any
	if err := json.Unmarshal(out2, &got); err != nil {
		t.Fatalf("--json output not valid JSON: %v\n%s", err, out2)
	}
	if got["id"] == nil {
		t.Fatalf("--json missing id field: %s", out2)
	}
}
