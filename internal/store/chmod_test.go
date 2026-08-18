package store

import (
	"os"
	"path/filepath"
	"testing"
)

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

// Regression for gh#123: symlink swap must not allow chmod to follow the
// symlink and modify an attacker-chosen target.
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

func TestRestoreWrite_RefusesHardlinkRace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	// 0o444: a file the pre-zssw loto left write-stripped.
	if err := os.WriteFile(target, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker")

	injectHardlinkOnce(t, target, attacker)

	if err := restoreWrite(target); err == nil {
		t.Fatal("restoreWrite must refuse when Nlink>1 on the open fd, got nil error")
	}
	// The shared inode must be untouched — attacker's name stays read-only.
	st, _ := os.Stat(attacker)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("attacker file gained owner-write via hardlink, mode=%o", st.Mode().Perm())
	}
}

// injectHardlinkOnce installs a one-shot afterOpenHook that hardlinks
// target→link on its first fire — simulating a racing process inside the
// restore TOCTOU window — then restores the previous hook. The hook is
// auto-restored on test cleanup.
func injectHardlinkOnce(t *testing.T, target, link string) {
	t.Helper()
	prev := afterOpenHook
	afterOpenHook = func(string) {
		if err := os.Link(target, link); err != nil {
			t.Fatalf("inject hardlink: %v", err)
		}
		afterOpenHook = prev // fire once
	}
	t.Cleanup(func() { afterOpenHook = prev })
}
