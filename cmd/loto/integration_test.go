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
	for i := 0; i < N; i++ {
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
