package store

import (
	"path/filepath"
	"testing"
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
	if _, err := s.db.Exec(`SELECT 1 FROM tags LIMIT 0`); err != nil {
		t.Fatalf("tags table missing: %v", err)
	}
	s.Close()

	s2, err := Open(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	s2.Close()
}
