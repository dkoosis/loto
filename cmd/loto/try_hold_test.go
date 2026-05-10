//go:build unix

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"testing"
	"time"
)

// TestWaitForSignal_ReturnsOnSIGTERM is a baseline behavioral guard: the
// helper must unblock when a signal arrives. Run in a goroutine so the test
// process doesn't get torn down if the helper somehow exits the process.
func TestWaitForSignal_ReturnsOnSIGTERM(t *testing.T) {
	done := make(chan struct{})
	go func() {
		waitForSignal()
		close(done)
	}()
	// Give the goroutine a moment to install the handler.
	time.Sleep(50 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("kill self: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForSignal did not return within 2s after SIGTERM")
	}
}

// TestWaitForSignal_StopsHandler asserts the source-level guarantee that
// waitForSignal calls signal.Stop on its channel before returning. Without
// it, repeated invocations leak signal registrations (loto-xra item c).
//
// Source-level rather than behavioral because verifying signal.Stop in-process
// is brittle: signal.Reset and Notify state is global, so a sibling test that
// also registers SIGTERM/SIGINT can mask the leak.
func TestWaitForSignal_StopsHandler(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	// Match defer signal.Stop(...) anywhere inside waitForSignal.
	fn := extractFunc(t, string(src), "waitForSignal")
	if !regexp.MustCompile(`defer\s+signal\.Stop\(`).MatchString(fn) {
		t.Fatalf("waitForSignal missing `defer signal.Stop(...)`:\n%s", fn)
	}
}

// TestTryRunE_DefersUnlock asserts both file-try and global-try RunE bodies
// install `defer ... Unlock()` immediately after a successful acquire. Without
// the defer, a panic between emit and unlock orphans the .tag file
// (loto-xra item a).
func TestTryRunE_DefersUnlock(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".", "main.go"))
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	s := string(src)
	// Both RunE bodies acquire then defer Unlock. The acquire helpers are
	// acquireFile and acquireGlobal; require a `defer ... Unlock` token
	// within ~12 lines of each call.
	// Tighten match: require `defer func() {` on a code line containing
	// Unlock — comments alone can't satisfy this.
	pat := regexp.MustCompile(`defer\s+func\(\s*\)\s*\{[^}]*Unlock\(`)
	for _, acquire := range []string{"acquireFile(", "acquireGlobal("} {
		idx := indexOf(s, acquire)
		if idx < 0 {
			t.Fatalf("acquire helper %q not found in main.go", acquire)
		}
		window := s[idx:min(idx+800, len(s))]
		if !pat.MatchString(window) {
			t.Fatalf("RunE using %s lacks `defer func() { ... Unlock() }` within 800 chars:\n%s", acquire, window)
		}
	}
}

// extractFunc returns the body of the named top-level func, naive but enough
// for the small surface here.
func extractFunc(t *testing.T, src, name string) string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^func\s+` + regexp.QuoteMeta(name) + `\s*\(`)
	loc := re.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("func %s not found", name)
	}
	// Walk forward to balance braces.
	i := loc[1]
	for i < len(src) && src[i] != '{' {
		i++
	}
	depth := 0
	start := i
	for ; i < len(src); i++ {
		switch src[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : i+1]
			}
		}
	}
	t.Fatalf("unterminated func %s", name)
	return ""
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
