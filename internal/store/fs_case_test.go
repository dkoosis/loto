package store

import (
	"path/filepath"
	"testing"
)

func TestFSCaseProbeAndCache(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got1, err := s.FSCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.FSCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got1 != got2 {
		t.Errorf("cached value mismatch: %v vs %v", got1, got2)
	}
}
