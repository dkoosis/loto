//go:build unix

package loto

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestReapRefusesHeldLock(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "edit", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()

	err = l.Reap("x.go")
	if err == nil {
		t.Fatal("Reap should refuse to clear a currently-held lock")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected ErrHeld, got %T: %v", err, err)
	}
	if held.Kind != "file" {
		t.Errorf("expected Kind=file, got %q", held.Kind)
	}
}

func TestReapClearsStaleTag(t *testing.T) {
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

	if err := l.Reap("x.go"); err != nil {
		t.Fatalf("Reap should clear stale tag: %v", err)
	}
	if _, err := l.ReadTag("x.go"); err == nil {
		t.Fatal("expected no tag after Reap")
	}
}

func TestReapIfDeadRefusesLiveProcess(t *testing.T) {
	l := newTestLOTO(t)
	_, err := l.TryFileLock("live", "work", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	// Don't unlock — live process holds it.

	err = l.ReapIfDead("x.go")
	if err == nil {
		t.Fatal("expected ErrHeld for live process")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *ErrHeld, got %T", err)
	}
}

func TestReapIfDeadClearsDeadPIDTag(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "work", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Unlock()

	// Plant a tag with PID 0 (never a real process).
	_, tagPath, _ := l.filePaths("x.go")
	deadTag := `{"agent_id":"ghost","pid":0,"target":"x.go","kind":"file","timestamp":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(tagPath, []byte(deadTag), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := l.ReapIfDead("x.go"); err != nil {
		t.Fatalf("ReapIfDead should succeed for dead PID: %v", err)
	}
	if _, err := l.ReadTag("x.go"); err == nil {
		t.Fatal("expected tag removed")
	}
}

func TestLazyGCOnAcquireAfterCrash(t *testing.T) {
	l := newTestLOTO(t)
	lock, err := l.TryFileLock("a", "work", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	_ = lock.Unlock()

	// Plant a stale tag with dead PID, simulating a crashed holder.
	_, tagPath, _ := l.filePaths("x.go")
	deadTag := `{"agent_id":"ghost","pid":0,"target":"x.go","kind":"file","timestamp":"2026-01-01T00:00:00Z"}`
	if err := os.WriteFile(tagPath, []byte(deadTag), 0o600); err != nil {
		t.Fatal(err)
	}

	// Next acquire should succeed and lazy-GC the stale tag.
	lock2, err := l.TryFileLock("b", "work", "x.go")
	if err != nil {
		t.Fatalf("acquire after crash should succeed: %v", err)
	}
	defer lock2.Unlock()

	tag, err := l.ReadTag("x.go")
	if err != nil {
		t.Fatalf("expected tag for new holder: %v", err)
	}
	if tag.AgentID != "b" {
		t.Errorf("expected new holder 'b', got %q", tag.AgentID)
	}
}

func TestErrHeldContainsTag(t *testing.T) {
	l := newTestLOTO(t)
	_, err := l.TryFileLock("holder-agent", "doing-work", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	// Don't unlock — leave it held.

	_, err = l.TryFileLock("other", "try", "x.go")
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *ErrHeld, got %T: %v", err, err)
	}
	if held.Kind != "file" {
		t.Errorf("Kind=%q, want file", held.Kind)
	}
	if held.Tag == nil {
		t.Fatal("expected Tag to be populated")
	}
	if held.Tag.AgentID != "holder-agent" {
		t.Errorf("AgentID=%q, want holder-agent", held.Tag.AgentID)
	}
}

func TestErrHeldGlobalContainsTag(t *testing.T) {
	l := newTestLOTO(t)
	_, err := l.TryGlobalLock("global-holder", "sweep")
	if err != nil {
		t.Fatal(err)
	}

	_, err = l.TryFileLock("other", "try", "x.go")
	if err == nil {
		t.Fatal("expected conflict error")
	}
	var held *ErrHeld
	if !errors.As(err, &held) {
		t.Fatalf("expected *ErrHeld, got %T: %v", err, err)
	}
	if held.Kind != "global" {
		t.Errorf("Kind=%q, want global", held.Kind)
	}
}

func TestAcquireSucceedsWhenFree(t *testing.T) {
	l := newTestLOTO(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	lock, err := l.Acquire(ctx, "a", "work", "x.go")
	if err != nil {
		t.Fatalf("Acquire on free target: %v", err)
	}
	defer lock.Unlock()
}

func TestAcquireTimesOutWhenHeld(t *testing.T) {
	l := newTestLOTO(t)
	holder, err := l.TryFileLock("holder", "hold", "x.go")
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	_, err = l.Acquire(ctx, "waiter", "wait", "x.go")
	if err == nil {
		t.Fatal("expected timeout error")
	}
	var sys *ErrSystem
	if !errors.As(err, &sys) {
		t.Fatalf("expected ErrSystem (context cancel), got %T: %v", err, err)
	}
}

func TestAcquireSucceedsAfterRelease(t *testing.T) {
	l := newTestLOTO(t)
	holder, err := l.TryFileLock("holder", "hold", "x.go")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		lock, err := l.Acquire(ctx, "waiter", "wait", "x.go")
		if err == nil {
			lock.Unlock()
		}
		done <- err
	}()

	time.Sleep(100 * time.Millisecond)
	holder.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("Acquire should succeed after release: %v", err)
	}
}

// TestForceBreak: agent-a acquires, agent-b force-breaks, agent-a finds the
// system message in their inbox.
func TestForceBreak(t *testing.T) {
	l := newTestLOTO(t)
	target := "contested.go"

	// agent-a acquires the lock.
	lockA, err := l.TryFileLock("agent-a", "edit", target)
	if err != nil {
		t.Fatalf("agent-a acquire: %v", err)
	}

	// agent-b force-breaks in a goroutine (it will block until agent-a releases).
	breakDone := make(chan error, 1)
	go func() {
		breakDone <- l.ForceBreak(target, "agent-b", "taking over for review")
	}()

	// Let agent-b reach the blocking flock call.
	time.Sleep(50 * time.Millisecond)

	// agent-a releases — unblocks agent-b's flock.
	if err := lockA.Unlock(); err != nil {
		t.Fatalf("agent-a unlock: %v", err)
	}

	if err := <-breakDone; err != nil {
		t.Fatalf("ForceBreak: %v", err)
	}

	// agent-a checks inbox: expects a system message from agent-b.
	msgs, err := l.ReadMsgs(target, "agent-a")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}
	if len(msgs) == 0 {
		t.Fatal("expected at least one inbox message for agent-a after ForceBreak")
	}
	m := msgs[0]
	if !m.System {
		t.Errorf("expected system message, got system=%v", m.System)
	}
	if m.From != "agent-b" {
		t.Errorf("expected From=agent-b, got %q", m.From)
	}
	if m.To != "agent-a" {
		t.Errorf("expected To=agent-a, got %q", m.To)
	}
}

// TestForceBreakNoHolder: ForceBreak on an unheld lock succeeds immediately
// (no tag to notify, no one to displace).
func TestForceBreakNoHolder(t *testing.T) {
	l := newTestLOTO(t)
	if err := l.ForceBreak("nobody.go", "agent-x", "cleanup"); err != nil {
		t.Fatalf("ForceBreak on unheld target: %v", err)
	}
}

func TestReserveAndList(t *testing.T) {
	l := newTestLOTO(t)
	_, err := l.Reserve("agent-a", "adding auth handler", "internal/auth/**", 0)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 reservation, got %d", len(got))
	}
	if got[0].Pattern != "internal/auth/**" {
		t.Fatalf("pattern = %q", got[0].Pattern)
	}
}

func TestUnreserveRemoves(t *testing.T) {
	l := newTestLOTO(t)
	if _, err := l.Reserve("agent-a", "intent", "src/**", 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if err := l.Unreserve("src/**"); err != nil {
		t.Fatalf("Unreserve: %v", err)
	}
	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 reservations after release, got %d", len(got))
	}
}

func TestReserveExpiredTTLPruned(t *testing.T) {
	l := newTestLOTO(t)
	if _, err := l.Reserve("agent-a", "temp work", "pkg/**", 1*time.Millisecond); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 (expired) reservations, got %d", len(got))
	}
}

// TestListReservationsQuarantinesCorruptTag: a malformed .tag file must be
// quarantined to a sidecar (with a stderr warning) rather than silently
// dropped. Valid reservations beside it are still returned. Regression for
// loto-ydi.
func TestListReservationsQuarantinesCorruptTag(t *testing.T) {
	l := newTestLOTO(t)
	if _, err := l.Reserve("agent-good", "intent", "src/**", 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	resDir := l.reservationsDir()
	corruptPath := filepath.Join(resDir, "deadbeef"+reservationExt)
	if err := os.WriteFile(corruptPath, []byte("{not valid json"), 0o600); err != nil {
		t.Fatalf("plant corrupt tag: %v", err)
	}

	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != "src/**" {
		t.Fatalf("want only the valid reservation, got %+v", got)
	}

	// Original corrupt tag should be gone — renamed to a .corrupt-* sidecar
	// so it's visible to operators but no longer participates in coordination.
	if _, err := os.Stat(corruptPath); !os.IsNotExist(err) {
		t.Errorf("corrupt tag should have been renamed; stat=%v", err)
	}
	entries, err := os.ReadDir(resDir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	foundQuarantine := false
	for _, e := range entries {
		if strings.Contains(e.Name(), ".corrupt-") {
			foundQuarantine = true
			break
		}
	}
	if !foundQuarantine {
		t.Errorf("expected quarantine sidecar in %s; entries=%v", resDir, entries)
	}
}

func TestTryFileLockSurfacesConflictingReservation(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "internal", "store", "db.go")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("package store"), 0o600); err != nil {
		t.Fatal(err)
	}

	absTarget, _ := filepath.Abs(target)
	pattern := filepath.Join(filepath.Dir(filepath.Dir(absTarget)), "**")
	if _, err := l.Reserve("agent-b", "refactoring store", pattern, 0); err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	lock, err := l.TryFileLock("agent-a", "test", target)
	if err != nil {
		t.Fatalf("TryFileLock: %v", err)
	}
	defer lock.Unlock()

	if len(lock.Conflicts) == 0 {
		t.Fatal("expected advisory conflict from reservation, got none")
	}
	if lock.Conflicts[0].AgentID != "agent-b" {
		t.Fatalf("conflicting agent = %q", lock.Conflicts[0].AgentID)
	}
}

func TestReserveInvalidPattern(t *testing.T) {
	l := newTestLOTO(t)
	_, err := l.Reserve("agent-a", "intent", "[\x00bad", 0)
	if err == nil {
		t.Fatal("expected error for invalid pattern")
	}
}
