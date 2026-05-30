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

// Regression for gh#123: symlink swap must not allow chmod to follow
// the symlink and modify an attacker-chosen target. stripWrite and
// restoreWrite must refuse symlinks.
func TestStripWrite_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := stripWrite(link); err == nil {
		t.Fatal("stripWrite must refuse symlink, got nil error")
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("target was modified via symlink, mode=%o", st.Mode().Perm())
	}
}

func TestRestoreWrite_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := restoreWrite(link); err == nil {
		t.Fatal("restoreWrite must refuse symlink, got nil error")
	}
	st, _ := os.Stat(target)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("target was modified via symlink, mode=%o", st.Mode().Perm())
	}
}

func TestStripWrite_RefusesDirectory(t *testing.T) {
	dir := t.TempDir()
	if err := stripWrite(dir); err == nil {
		t.Fatal("stripWrite must refuse directory")
	}
}

// Regression for loto-ta02: hardlink TOCTOU between validateFileTarget
// (one-shot Lstat Nlink<=1) and stripWrite. A second hardlink created
// after validation but before fchmod makes the strip clear write bits on
// an attacker-chosen name on the shared inode. stripWrite must re-check
// Nlink on the open fd and refuse when Nlink>1.
//
// afterOpenHook fires inside stripWrite right after the fd is opened,
// simulating the racing process that hardlinks the locked target. This
// makes the TOCTOU deterministic instead of relying on a real race.
func TestStripWrite_RefusesHardlinkRace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker")

	prev := afterOpenHook
	afterOpenHook = func(string) {
		// Racing process hardlinks the locked inode to a name it owns.
		if err := os.Link(target, attacker); err != nil {
			t.Fatalf("inject hardlink: %v", err)
		}
		afterOpenHook = prev // fire once
	}
	defer func() { afterOpenHook = prev }()

	if err := stripWrite(target); err == nil {
		t.Fatal("stripWrite must refuse when Nlink>1 on the open fd, got nil error")
	}
	// The shared inode must be untouched — attacker's name keeps write bits.
	st, _ := os.Stat(attacker)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("attacker file write-stripped via hardlink, mode=%o", st.Mode().Perm())
	}
}
