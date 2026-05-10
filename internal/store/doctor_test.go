package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestDoctorListsStaleLocks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkLock("a.go", "alice", time.Hour)
	if _, err := s.AcquireLock(ctx, l, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, l.Host, dead)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleLocks) != 1 {
		t.Fatalf("expected 1 stale lock, got %d", len(report.StaleLocks))
	}
}

func TestDoctorRepairReclaims(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkLock("a.go", "alice", time.Hour)
	if _, err := s.AcquireLock(ctx, l, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, l.Host, "doctor-agent", dead); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("stale lock should be reclaimed, got %+v", got)
	}
}

func TestMoveCorruptDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	s, _ := Open(dbPath)
	s.Close()

	moved, err := MoveCorruptAside(dbPath, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if moved == "" {
		t.Fatal("expected moved path")
	}
}
