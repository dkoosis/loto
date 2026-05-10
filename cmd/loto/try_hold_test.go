//go:build unix

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestInstallSignalHandler_ReceivesSIGTERM is a baseline behavioral guard:
// the channel returned must receive when a signal arrives. Run the receive in
// a goroutine so the test process isn't torn down if the helper somehow
// exits the process.
func TestInstallSignalHandler_ReceivesSIGTERM(t *testing.T) {
	c, stop := installSignalHandler()
	defer stop()
	done := make(chan struct{})
	go func() {
		<-c
		close(done)
	}()
	// Give the goroutine a moment to start blocking.
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("installSignalHandler channel did not receive SIGTERM within 2s")
	}
}

// TestTryRunE_HandlerOutlivesUnlock asserts the source-level invariant that
// in the --hold path, both file-try and global-try RunE bodies register
// `defer stop()` BEFORE `defer ... Unlock()`. LIFO defer ordering then runs
// Unlock first and signal.Stop second, keeping the handler live across the
// caller's cleanup. Without this ordering a second SIGINT arriving mid-unlock
// would fall through to Go's default handler (loto-xra item b).
//
// Source-level rather than behavioral because verifying handler lifetime
// in-process is brittle: signal.Notify state is global and a sibling test
// that also registers SIGTERM/SIGINT can mask ordering bugs.
func TestTryRunE_HandlerOutlivesUnlock(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	s := string(src)
	unlockPat := regexp.MustCompile(`defer\s+func\(\s*\)\s*\{[^}]*Unlock\(`)
	for _, acquire := range []string{"acquireFile(", "acquireGlobal("} {
		idx := strings.Index(s, acquire)
		if idx < 0 {
			t.Fatalf("acquire helper %q not found in main.go", acquire)
		}
		end := min(idx+1200, len(s))
		window := s[idx:end]
		stopIdx := strings.Index(window, "defer stop()")
		unlockLoc := unlockPat.FindStringIndex(window)
		if stopIdx < 0 {
			t.Fatalf("RunE using %s missing `defer stop()` (handler cleanup):\n%s", acquire, window)
		}
		if unlockLoc == nil {
			t.Fatalf("RunE using %s missing `defer ... Unlock()`:\n%s", acquire, window)
		}
		if stopIdx > unlockLoc[0] {
			t.Fatalf("RunE using %s registers `defer stop()` after `defer Unlock()` — LIFO would run stop first and break the handler-outlives-unlock invariant:\n%s", acquire, window)
		}
	}
}
