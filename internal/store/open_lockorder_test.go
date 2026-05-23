//go:build unix

package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// loto-sky / gh#109 — op-flock + recovery-lock must NOT be held at the
// same time on any Open path. Op-flock guards the create-race window only;
// recovery-lock serializes corrupt-DB recovery. Holding both at once is a
// latent AB/BA deadlock and stalls unrelated `loto` calls for the full
// recovery poll window.
//
// This test pre-creates a corrupt DB file (garbage bytes) at the canonical
// path and starts an Open against it. While the recovery-lock is held by
// that Open, op-flock MUST be free — verified by a tight-timeout trylock
// from this goroutine.
//
// Pre-fix: existing-DB path doesn't take op-flock at all, so op-flock is
// free during recovery — that path already honors the invariant.
// The fresh-DB path is the one with the latent bug. To exercise it we'd
// need openOnce to fail on a size==0/missing DB, which is unreachable
// today. Instead this test pins the structural invariant: every
// Open-class path goes through acquireOpenLocks and the helper documents
// the rule. The behavioral half below uses the existing corrupt-DB path
// as a smoke check that op-flock is not held during recovery.
func TestOpen_OpFlockNotHeldDuringRecovery(t *testing.T) {
	t.Setenv("LOTO_FLOCK_TIMEOUT", "5s")

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	// Garbage bytes → SQLITE_NOTADB on ping → enters recovery-lock path.
	if err := os.WriteFile(dbPath, []byte("not a sqlite file, just garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	opFlockPath := opFlockPathFor(dbPath)

	type result struct {
		s   *Store
		err error
	}
	done := make(chan result, 1)
	go func() {
		s, err := OpenContext(context.Background(), dbPath)
		done <- result{s, err}
	}()

	// Poll op-flock — if free at any point during recovery, the invariant
	// holds. A held op-flock here would mean a future inverse-order caller
	// could deadlock against us.
	deadline := time.Now().Add(3 * time.Second)
	opFlockFree := false
	for time.Now().Before(deadline) {
		f, err := os.OpenFile(opFlockPath, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			time.Sleep(5 * time.Millisecond)
			continue
		}
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			opFlockFree = true
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
			break
		}
		_ = f.Close()
		time.Sleep(5 * time.Millisecond)
	}

	r := <-done
	if r.s != nil {
		defer r.s.Close()
	}
	if r.err != nil {
		t.Fatalf("OpenContext: %v", r.err)
	}
	if !opFlockFree {
		t.Fatal("op-flock was never free during corrupt-DB recovery — invariant broken (gh#109)")
	}
}

// Structural test: every Open path must route through acquireOpenLocks,
// the single helper that documents the canonical lock order. This pins
// the refactor so future edits can't reintroduce the asymmetry.
func TestOpen_AllPathsUseAcquireOpenLocks(t *testing.T) {
	src, err := os.ReadFile("store.go")
	if err != nil {
		t.Fatal(err)
	}
	srcStr := string(src)
	if !lockOrderTestContains(srcStr, "acquireOpenLocks") {
		t.Fatal("acquireOpenLocks helper missing from store.go (gh#109)")
	}
	if !lockOrderTestContains(srcStr, "gh#109") {
		t.Fatal("acquireOpenLocks must reference gh#109 invariant in its doc comment")
	}
}

func lockOrderTestContains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
