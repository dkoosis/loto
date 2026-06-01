//go:build unix

package store

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// loto-8yst / gh#109 — a panic inside openOnce on the fresh-DB path must
// NOT leak the op-flock fd. The explicit release at the end of the fresh-DB
// gap is skipped when the stack unwinds through a panic; a deferred release
// (safety net) must still free the lock. In a single-process `go test` run a
// stranded op-flock fd would wedge every sibling test on the same path until
// the binary exits.
//
// This test injects a panic via openOnceHook while opening a fresh
// (nonexistent) DB, recovers it, then asserts the op-flock is immediately
// acquirable with a non-blocking trylock — proving it was released despite
// the panic.
func TestOpen_PanicInGapDoesNotLeakOpFlock(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	opFlockPath := opFlockPathFor(dbPath)

	prev := openOnceHook
	openOnceHook = func() { panic("injected panic in openOnce gap") }
	t.Cleanup(func() { openOnceHook = prev })

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected injected panic from openOnceHook, got none")
			}
		}()
		_, _ = OpenContext(context.Background(), dbPath)
	}()

	// op-flock must be free now. A non-blocking trylock that succeeds proves
	// the deferred release ran during the panic unwind.
	f, err := os.OpenFile(opFlockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open op-flock file: %v", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("op-flock still held after panic in openOnce gap — fd leaked (gh#109): %v", err)
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
