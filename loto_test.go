//go:build unix

package loto

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestLOTO(t *testing.T) *LOTO {
	t.Helper()
	l, err := New(filepath.Join(t.TempDir(), "coord"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// Same logical file, three spellings: relative, dotted-relative, absolute.
// All must collide on the same lock — otherwise the LOTO invariant leaks.
func TestPathNormalizationCollides(t *testing.T) {
	l := newTestLOTO(t)
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	abs := filepath.Join(cwd, "src/foo.go")

	a, err := l.TryFileLock("agent-a", "edit", "src/foo.go")
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	defer a.Unlock()

	if _, err := l.TryFileLock("agent-b", "edit", "./src/foo.go"); err == nil {
		t.Fatal("expected ./src/foo.go to collide with src/foo.go")
	}
	if _, err := l.TryFileLock("agent-b", "edit", abs); err == nil {
		t.Fatal("expected absolute path to collide with relative path")
	}
}

func TestFileLockBlocksGlobal(t *testing.T) {
	l := newTestLOTO(t)
	f, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatalf("file lock: %v", err)
	}
	defer f.Unlock()

	if _, err := l.TryGlobalLock("b", "release"); err == nil {
		t.Fatal("global lock should fail while a file lock is held")
	}
}

func TestGlobalLockBlocksFile(t *testing.T) {
	l := newTestLOTO(t)
	g, err := l.TryGlobalLock("a", "release")
	if err != nil {
		t.Fatalf("global lock: %v", err)
	}
	defer g.Unlock()

	if _, err := l.TryFileLock("b", "edit", "x.go"); err == nil {
		t.Fatal("file lock should fail while global lock is held")
	}
}

func TestUnlockClearsTag(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatalf("lock: %v", err)
	}

	if _, err := l.ReadTag("x.go"); err != nil {
		t.Fatalf("expected tag while held: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, err := l.ReadTag("x.go"); err == nil {
		t.Fatal("expected no tag after unlock")
	}
}

func TestUnlockIsIdempotent(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatalf("lock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("first unlock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("second unlock should be a no-op: %v", err)
	}
}

func TestTwoFileLocksOnDifferentTargetsCoexist(t *testing.T) {
	l := newTestLOTO(t)
	a, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Unlock()
	b, err := l.TryFileLock("b", "edit", "y.go")
	if err != nil {
		t.Fatalf("disjoint file locks should coexist: %v", err)
	}
	defer b.Unlock()
}

func TestBreakRefusesHeldLock(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	if err := l.Break("x.go"); err == nil {
		t.Fatal("Break should refuse to clear a currently-held lock")
	}
}

func TestBreakClearsStaleTag(t *testing.T) {
	l := newTestLOTO(t)
	// Acquire and release, but manually plant a tag to simulate a crashed
	// holder that never cleaned up.
	lock, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatal(err)
	}
	_, tagPath, err := l.filePaths("x.go")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tagPath, []byte(`{"agent_id":"ghost"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := l.Break("x.go"); err != nil {
		t.Fatalf("Break should clear stale tag: %v", err)
	}
	if _, err := l.ReadTag("x.go"); err == nil {
		t.Fatal("expected no tag after Break")
	}
}
