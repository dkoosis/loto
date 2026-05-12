package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
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

// isCorruptDB must trip on real sqlite NOTADB/CORRUPT errors only — not on
// arbitrary wrapped errors that happen to contain the substring "malformed".
// Regression: gh#48 — string-match isCorruptDB destroys DB on false positives.

func TestIsCorruptDB_RealNotADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(path, []byte("not a sqlite file, just garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pingErr := db.PingContext(context.Background())
	if pingErr == nil {
		t.Fatal("expected ping to fail on garbage file")
	}
	if !isCorruptDB(pingErr) {
		t.Fatalf("isCorruptDB must recognize real SQLITE_NOTADB, got: %v", pingErr)
	}
}

var (
	errSpoofMalformed = errors.New("transient network read: database disk image is malformed (cached)")
	errSpoofNotADB    = errors.New("file is not a database (from middleware)")
)

func TestIsCorruptDB_NotFooledBySubstring(t *testing.T) {
	// Plain wrapped errors containing corrupt-shaped substrings must NOT
	// trip corrupt detection — only real sqlite errno codes do.
	if isCorruptDB(fmt.Errorf("wrap: %w", errSpoofMalformed)) {
		t.Fatal("isCorruptDB false-positive on substring match — would destroy a healthy DB")
	}
	if isCorruptDB(errSpoofNotADB) {
		t.Fatal("isCorruptDB false-positive on substring match")
	}
}

// MoveCorruptAside must be all-or-nothing: either every existing sibling
// (db, -wal, -shm) is moved aside together, or nothing moves. A concurrent
// opener must never see a fresh loto.db paired with a stale -wal.

func TestMoveCorruptAsideAtomic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Force WAL+SHM into existence with a write.
	if _, err := s.db.ExecContext(context.Background(), `CREATE TABLE tmp(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		if _, err := os.Stat(dbPath + sfx); err != nil {
			// -wal/-shm may not exist after Close; that's fine. Re-create to test.
			if sfx != "" {
				_ = os.WriteFile(dbPath+sfx, []byte("sidecar"), 0o600)
			}
		}
	}

	when := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	moved, err := MoveCorruptAside(dbPath, when)
	if err != nil {
		t.Fatalf("MoveCorruptAside: %v", err)
	}

	// After move-aside: the original three names must all be gone together.
	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		if _, err := os.Stat(dbPath + sfx); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, stat err=%v", dbPath+sfx, err)
		}
	}
	// And the move-aside artifact must hold all three.
	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		want := filepath.Join(moved, "loto.db"+sfx)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected %s in moved dir, stat err=%v", want, err)
		}
	}
}
