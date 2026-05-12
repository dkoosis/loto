package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStripWrite_RemovesAllWriteBits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o664); err != nil {
		t.Fatal(err)
	}
	if err := stripWrite(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("expected no write bits, got %o", st.Mode().Perm())
	}
}

func TestRestoreWrite_AddsOwnerWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := restoreWrite(p); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected owner write, got %o", st.Mode().Perm())
	}
}

func TestRestoreWrite_MissingFileIsNoop(t *testing.T) {
	if err := restoreWrite(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("missing file should be noop, got %v", err)
	}
}
