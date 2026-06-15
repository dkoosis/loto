package store

import (
	"os"
	"path/filepath"
	"testing"

	"loto/internal/domain"
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

// injectHardlinkOnce installs a one-shot afterOpenHook that hardlinks
// target→link on its first fire — simulating a racing process inside the
// strip/restore TOCTOU window — then restores the previous hook. The hook is
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

	injectHardlinkOnce(t, target, attacker)

	if err := stripWrite(target); err == nil {
		t.Fatal("stripWrite must refuse when Nlink>1 on the open fd, got nil error")
	}
	// The shared inode must be untouched — attacker's name keeps write bits.
	st, _ := os.Stat(attacker)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("attacker file write-stripped via hardlink, mode=%o", st.Mode().Perm())
	}
}

// Regression for loto-pduc: the restore side has the same hardlink TOCTOU as
// the strip side (loto-ta02). Between the validated strip at acquire and the
// later restore at release/break/reclaim, a racing process can hardlink the
// locked inode to a name it owns. safeOpenRegular accepts it (regular file),
// then restoreWrite would add owner-write to the SHARED inode — silently
// making the attacker's read-only file writable. restoreWrite must re-check
// Nlink on the open fd and refuse when Nlink>1, mirroring stripWrite.
//
// afterOpenHook fires inside restoreWrite right after the fd is opened,
// simulating the racing process. This makes the TOCTOU deterministic.
func TestRestoreWrite_RefusesHardlinkRace(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	// 0o444: the locked file was write-stripped at acquire.
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

// Regression for loto-pduc: a restore-side hardlink race must surface through
// the caller plumbing — restoreReleases+auditReleaseFailures flip the result to
// StateRestoreFailed and emits a mode_restore_failed audit event. This proves
// the errMultiLinked guard reaches the audit trail end-to-end (acceptance
// criterion), not just the fd-level refusal.
func TestRestoreAndAuditReleases_HardlinkRaceEmitsModeRestoreFailed(t *testing.T) {
	s := mustOpen(t)

	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	attacker := filepath.Join(dir, "attacker")

	injectHardlinkOnce(t, p, attacker)

	results := []ReleaseResult{
		{Target: domain.Target{Canonical: p}, State: StateUnlocked},
	}
	failEvents, failIdx := restoreReleases(results, tcAlice)
	s.auditReleaseFailures(results, failEvents, failIdx)

	if results[0].State != StateRestoreFailed {
		t.Fatalf("want StateRestoreFailed on Nlink>1, got %v", results[0].State)
	}
	if results[0].RestoreErr == nil {
		t.Fatal("RestoreErr nil — restoreWrite must refuse Nlink>1")
	}

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE target_canonical=? AND event_kind='mode_restore_failed'`, p,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("want 1 mode_restore_failed event for %s, got %d", p, n)
	}
}
