package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenAppliesSchemaIdempotently(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.db.Exec(`SELECT 1 FROM locks LIMIT 0`); err != nil {
		t.Fatalf("locks table missing: %v", err)
	}
	if _, err := s.db.Exec(`SELECT 1 FROM events LIMIT 0`); err != nil {
		t.Fatalf("events table missing: %v", err)
	}
	s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}

func TestOpen_WipesOnUserVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE old_locks(target_canonical TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO old_locks VALUES ('stale.go')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	locks, err := s.ListLocks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 0 {
		t.Errorf("expected wiped DB, got %d locks", len(locks))
	}

	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Errorf("expected 1 aside file, got %d", len(matches))
	}
}

func TestTxBusyTimeoutMs(t *testing.T) {
	now := time.Unix(1_000_000_000, 0)

	if got := txBusyTimeoutMs(context.Background(), now); got != txBusyTimeoutDefaultMs {
		t.Errorf("no-deadline: got %d, want %d", got, txBusyTimeoutDefaultMs)
	}

	ctxShort, cancel := context.WithDeadline(context.Background(), now.Add(500*time.Microsecond))
	defer cancel()
	if got := txBusyTimeoutMs(ctxShort, now); got != 1 {
		t.Errorf("sub-ms deadline: got %d, want 1", got)
	}

	ctxMid, cancel2 := context.WithDeadline(context.Background(), now.Add(250*time.Millisecond))
	defer cancel2()
	if got := txBusyTimeoutMs(ctxMid, now); got != 250 {
		t.Errorf("250ms deadline: got %d, want 250", got)
	}

	ctxLong, cancel3 := context.WithDeadline(context.Background(), now.Add(10*time.Minute))
	defer cancel3()
	if got := txBusyTimeoutMs(ctxLong, now); got != txBusyTimeoutCapMs {
		t.Errorf("10min deadline: got %d, want %d (cap)", got, txBusyTimeoutCapMs)
	}
}

func TestStore_OpFlockPathDerivedFromDBPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	want := filepath.Join(dir, "lock-op.flock")
	if got := s.opFlockPath(); got != want {
		t.Errorf("opFlockPath = %q, want %q", got, want)
	}
}
