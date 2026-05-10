//go:build unix

package loto

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestAcquirePathBlocksCrossAgentTry: agent A acquires a path with TTL;
// agent B's TryFileLock returns ErrHeld surfacing A's identity even
// though the holder process (this same test process) holds no flock on
// the file.
func TestAcquirePathBlocksCrossAgentTry(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "shared.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tag, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute)
	if err != nil {
		t.Fatalf("AcquirePath agent-A: %v", err)
	}
	if tag == nil || tag.AgentID != "agent-A" {
		t.Fatalf("AcquirePath returned tag = %+v, want AgentID agent-A", tag)
	}
	if tag.ExpiresAt.IsZero() {
		t.Fatal("AcquirePath returned tag with zero ExpiresAt; want TTL-bounded")
	}

	_, err = l.TryFileLock("agent-B", "write", target)
	if err == nil {
		t.Fatal("TryFileLock agent-B returned nil error; want ErrHeld surfacing agent-A")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("TryFileLock agent-B returned %T %v; want *ErrHeld", err, err)
	}
	if held.Tag == nil || held.Tag.AgentID != "agent-A" {
		t.Fatalf("ErrHeld.Tag = %+v; want AgentID agent-A", held.Tag)
	}
}

// TestAcquirePathSameAgentReturnsTag: same-agent re-acquire extends TTL.
func TestAcquirePathSameAgentReturnsTag(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "self.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, _, err := l.AcquirePath("agent-A", "write", target, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first AcquirePath: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	second, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute)
	if err != nil {
		t.Fatalf("second AcquirePath (same agent): %v", err)
	}
	if !second.ExpiresAt.After(first.ExpiresAt) {
		t.Fatalf("re-acquire did not extend ExpiresAt: first=%v second=%v",
			first.ExpiresAt, second.ExpiresAt)
	}
}

// TestSameAgentTryDoesNotClobberRecordTier: an AcquirePath sets a record-tier
// tag with TTL; a same-agent TryFileLock followed by Unlock must NOT remove
// the tag. After Unlock the path is still authoritatively held until TTL
// expires. (bead loto-c4f / gh-31)
func TestSameAgentTryDoesNotClobberRecordTier(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "record.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Record-tier acquire by agent-A.
	if _, _, err := l.AcquirePath("agent-A", "edit", target, 5*time.Minute); err != nil {
		t.Fatalf("AcquirePath: %v", err)
	}
	tagBefore, err := l.ReadTag(target)
	if err != nil {
		t.Fatalf("ReadTag before: %v", err)
	}
	if !tagBefore.IsRecordTier() {
		t.Fatalf("expected record-tier tag, got %+v", tagBefore)
	}

	// 2. Same agent foreground try, then unlock.
	lock, err := l.TryFileLock("agent-A", "test", target)
	if err != nil {
		t.Fatalf("same-agent TryFileLock: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	// 3. Tag must survive — TTL authority preserved.
	tagAfter, err := l.ReadTag(target)
	if err != nil {
		t.Fatalf("ReadTag after Unlock: %v (tag should still be present)", err)
	}
	if !tagAfter.IsRecordTier() {
		t.Fatalf("tag lost record-tier status; got %+v", tagAfter)
	}
	if !tagAfter.ExpiresAt.Equal(tagBefore.ExpiresAt) {
		t.Fatalf("ExpiresAt changed across foreground try: before=%v after=%v",
			tagBefore.ExpiresAt, tagAfter.ExpiresAt)
	}

	// 4. Cross-agent try must still see the record-tier tag and be denied.
	if _, err := l.TryFileLock("agent-B", "intrude", target); err == nil {
		t.Fatal("cross-agent TryFileLock should be blocked by surviving record-tier tag")
	}
}

// TestAcquirePathTTLExpiry: after TTL expires, another agent's try succeeds.
func TestAcquirePathTTLExpiry(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "expiring.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.AcquirePath("agent-A", "brief", target, 50*time.Millisecond); err != nil {
		t.Fatalf("AcquirePath agent-A: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	lock, err := l.TryFileLock("agent-B", "write", target)
	if err != nil {
		t.Fatalf("TryFileLock agent-B after TTL expiry: %v", err)
	}
	if lock == nil {
		t.Fatal("TryFileLock returned nil lock with nil error")
	}
	defer lock.Unlock()
}

// TestReleasePathSameAgent: release clears, subsequent try succeeds.
func TestReleasePathSameAgent(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "rel.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute); err != nil {
		t.Fatalf("AcquirePath: %v", err)
	}
	if err := l.ReleasePath("agent-A", target); err != nil {
		t.Fatalf("ReleasePath same agent: %v", err)
	}
	lock, err := l.TryFileLock("agent-B", "write", target)
	if err != nil {
		t.Fatalf("TryFileLock after release: %v", err)
	}
	defer lock.Unlock()
}

// TestReleasePathCrossAgentRejected: release by non-holder returns ErrNotMine.
func TestReleasePathCrossAgentRejected(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "rel.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute); err != nil {
		t.Fatalf("AcquirePath: %v", err)
	}
	err := l.ReleasePath("agent-B", target)
	if err == nil {
		t.Fatal("ReleasePath by non-holder returned nil; want ErrNotMine")
	}
	var notMine *ErrNotMine
	if !errors.As(err, &notMine) {
		t.Fatalf("ReleasePath by non-holder returned %T %v; want *ErrNotMine", err, err)
	}
}

// TestReleasePathUnheldIsSilentSuccess: per bead, idempotent for hook robustness.
func TestReleasePathUnheldIsSilentSuccess(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "never-acquired.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := l.ReleasePath("agent-A", target); err != nil {
		t.Fatalf("ReleasePath unheld: %v; want nil", err)
	}
}

// TestAcquirePathWhileForegroundFlockHeld: while another agent holds via
// TryFileLock (foreground), AcquirePath returns ErrHeld with that holder.
func TestAcquirePathWhileForegroundFlockHeld(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "foreground.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	lock, err := l.TryFileLock("agent-fg", "edit", target)
	if err != nil {
		t.Fatalf("setup TryFileLock: %v", err)
	}
	defer lock.Unlock()

	_, _, err = l.AcquirePath("agent-bg", "write", target, 5*time.Minute)
	if err == nil {
		t.Fatal("AcquirePath while foreground flock held returned nil; want ErrHeld")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("AcquirePath returned %T %v; want *ErrHeld", err, err)
	}
	if held.Tag == nil || held.Tag.AgentID != "agent-fg" {
		t.Fatalf("ErrHeld.Tag = %+v; want AgentID agent-fg", held.Tag)
	}
}

// TestAcquirePathReturnsConflictingReservations: when a reservation matches
// target, AcquirePath returns it as advisory.
func TestAcquirePathReturnsConflictingReservations(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "internal", "store", "store.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package store\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := l.Reserve("agent-res", "store refactor", filepath.Join(filepath.Dir(target), "*"), time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	_, conflicts, err := l.AcquirePath("agent-A", "write", target, time.Minute)
	if err != nil {
		t.Fatalf("AcquirePath: %v", err)
	}
	if len(conflicts) == 0 {
		t.Fatal("AcquirePath returned no conflicts; want at least one (matching reservation)")
	}
}

// TestAcquirePathCrossProcessSameAgent: simulate a different process under
// same agentID by writing a tag with a foreign PID and re-acquiring. Must
// succeed (same identity = same agent across process boundaries).
func TestAcquirePathCrossProcessSameAgent(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "xproc.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute); err != nil {
		t.Fatalf("first AcquirePath: %v", err)
	}

	// Simulate a different process: rewrite the tag with PID=1 (init,
	// effectively foreign — alive but not us). agentID stays the same.
	_, fileTagPath, err := l.filePaths(target)
	if err != nil {
		t.Fatal(err)
	}
	t1, status := loadTag(fileTagPath)
	if status != tagOK {
		t.Fatalf("loadTag: status=%v", status)
	}
	t1.PID = 1
	if err := writeTagAtomic(fileTagPath, t1); err != nil {
		t.Fatal(err)
	}

	// Same agentID should succeed (extends TTL); not surface ErrHeld.
	if _, _, err := l.AcquirePath("agent-A", "write", target, 5*time.Minute); err != nil {
		t.Fatalf("cross-process same-agent re-acquire: %v; want success", err)
	}
}
