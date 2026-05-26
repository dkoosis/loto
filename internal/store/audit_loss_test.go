package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"loto/internal/domain"
)

const opFchmod = "fchmod" // goconst: shared across fchmodFn stubs below

// Regression for gh#107 (loto-27y): when the post-commit restore-failure
// audit write fails, the loss must NOT be silent. Pre-fix, callers used
// `_ = s.appendAuditDetached(...)` so any tx contention / SQLITE_BUSY /
// ctx-tail deadline silently dropped the mode_restore_failed event.
//
// We force the audit write to fail by closing the underlying DB after the
// release/break tx commits but before the post-commit restore-audit phase
// runs. Driver: invoke restoreAndAudit* directly with realPaths whose chmod
// is broken via fchmodFn so they yield RestoreErr. The audit attempt then
// fires against a closed DB and must surface via:
//   - stderr line containing "audit-write failed"
//   - per-result AuditErr field populated

func TestRestoreAndAuditReleases_AuditFailureSurfacesPerResult(t *testing.T) {
	s := mustOpen(t)
	var stderr bytes.Buffer
	s.SetStderr(&stderr)

	// Build a fake StateUnlocked result for a path whose chmod restore will
	// fail, then close the DB so the audit insert fails.
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == p {
			return &os.PathError{Op: opFchmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	results := []ReleaseResult{
		{Target: domain.Target{Canonical: p}, State: StateUnlocked},
	}
	s.restoreAndAuditReleases(results, tcAlice)

	if results[0].State != StateRestoreFailed {
		t.Fatalf("want StateRestoreFailed, got %v", results[0].State)
	}
	if results[0].RestoreErr == nil {
		t.Fatal("RestoreErr nil — restoreWrite should have failed via fchmodFn")
	}
	if results[0].AuditErr == nil {
		t.Fatal("AuditErr nil — audit write should fail against closed DB (gh#107)")
	}
	if !strings.Contains(stderr.String(), "audit-write failed") {
		t.Errorf("stderr missing audit-write failed line: %q", stderr.String())
	}
}

func TestRestoreAndAuditBreaks_AuditFailureSurfacesPerResult(t *testing.T) {
	s := mustOpen(t)
	var stderr bytes.Buffer
	s.SetStderr(&stderr)

	dir := t.TempDir()
	p := filepath.Join(dir, "y.go")
	if err := os.WriteFile(p, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == p {
			return &os.PathError{Op: opFchmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	if err := s.db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	results := []BreakResult{
		{Target: domain.Target{Canonical: p}, Err: nil},
	}
	s.restoreAndAuditBreaks(results, tcBob, time.Now())

	if results[0].RestoreErr == nil {
		t.Fatal("RestoreErr nil — restoreWrite should have failed via fchmodFn")
	}
	if results[0].AuditErr == nil {
		t.Fatal("AuditErr nil — audit write should fail against closed DB (gh#107)")
	}
	if !strings.Contains(stderr.String(), "audit-write failed") {
		t.Errorf("stderr missing audit-write failed line: %q", stderr.String())
	}
}

// In-tx variant: when stripAndHandleFailure encounters chmod-restore errors,
// the audit must be committed atomically with the parent acquire tx (gh#107).
// Pre-fix, the tx was rolled back BEFORE the detached audit write — so a
// concurrent writer holding the DB lock through audit's 2s budget would
// silently drop the trail. Post-fix, the audit rides inside the parent tx.
//
// Verify by observing that even though the acquire fails with
// ChmodFailureError, the mode_restore_failed event lands in the events table
// — and lands BEFORE any post-commit detached write would have a chance.
func TestStripAndHandleFailure_AuditCommittedInTx(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go")
	b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := mustOpen(t)

	// b's strip fails outright; a was stripped, but restoring a's write
	// bit also fails — pushing it onto restoreErrs path.
	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		switch {
		case f.Name() == b:
			return &os.PathError{Op: opFchmod, Path: f.Name(), Err: syscall.EPERM}
		case f.Name() == a && mode.Perm()&0o200 != 0:
			return &os.PathError{Op: opFchmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	live := func(string, int) bool { return true }
	now := time.Now()
	mk := func(p string) domain.LockRecord {
		return domain.LockRecord{
			Target:      domain.Target{Canonical: p},
			OwnerUUID:   tcAlice,
			SessionUUID: "s1",
			CreatedAt:   now,
			ExpiresAt:   now.Add(time.Hour),
			Host:        "h",
			PID:         1,
		}
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{mk(a), mk(b)}, live); err == nil {
		t.Fatal("expected ChmodFailureError")
	}

	// Audit must be present — committed atomically with the failed acquire.
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE target_canonical=? AND event_kind='mode_restore_failed'`, a,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("audit not committed atomically (gh#107): want 1 mode_restore_failed for %s, got %d", a, n)
	}
}
