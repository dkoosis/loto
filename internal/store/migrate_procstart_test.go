package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"loto/internal/domain"
)

// TestMigrate_AddsProcStartInPlace verifies the additive upgrade for DBs that
// predate the proc_start column (loto-kwlp): the column is added in-place and
// existing lock rows survive — NOT wiped via the user_version move-aside path.
func TestMigrate_AddsProcStartInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	// Build a pre-proc_start DB at the current schema version with one lock row.
	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE locks (
	  target_canonical TEXT PRIMARY KEY,
	  owner_uuid       TEXT NOT NULL,
	  session_uuid     TEXT NOT NULL,
	  intent           TEXT NOT NULL DEFAULT '',
	  created_at       INTEGER NOT NULL,
	  expires_at       INTEGER NOT NULL,
	  host             TEXT NOT NULL,
	  pid              INTEGER NOT NULL,
	  branch           TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO locks(target_canonical,owner_uuid,session_uuid,created_at,expires_at,host,pid)
	  VALUES ('a.go','alice','sess',1,2,'h',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaUserVersion)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Open runs migrate(): version matches (no wipe), guarded ALTER adds the col.
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Pre-existing row survived.
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM locks WHERE target_canonical='a.go'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pre-existing lock row count = %d, want 1 (rows must NOT be wiped)", n)
	}

	// proc_start column now exists and is NULL for the legacy row → unknown (0).
	got, err := s.LockAt(context.Background(), domain.Target{Canonical: "a.go"})
	if err != nil || got == nil {
		t.Fatalf("LockAt: %v / %v", got, err)
	}
	if got.ProcStart != 0 {
		t.Fatalf("legacy row ProcStart = %d, want 0 (NULL→unknown)", got.ProcStart)
	}

	// Re-Open is idempotent: guarded ALTER must not error on the second pass.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen (idempotent ALTER): %v", err)
	}
	s2.Close()
}
