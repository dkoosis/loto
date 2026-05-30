package store

import (
	"bytes"
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"loto/internal/domain"
)

// Regression for loto-rmyg: on a tx.Commit() failure, AcquireLocks calls
// restoreAllAndAudit while the parent tx still holds the SQLite write lock
// (its rollback is deferred). The detached audit then opens a SECOND write tx
// that self-contends against the held lock, stalling ~2s on busy_timeout and
// dropping the acquire_rollback_started breadcrumb. The fix releases the
// parent tx (cleanup) before the restore-audit, so the breadcrumb lands fast.
func TestAcquireLocks_CommitFailureBreadcrumbLandsWithoutSelfContention(t *testing.T) {
	s := mustOpen(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "c.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Inject a commit failure (disk-full / SQLITE_IOERR class) without a real
	// I/O fault. The real tx stays open, so the write lock is still held when
	// the restore-audit runs — exactly the self-contention condition.
	origCommit := commitTxFn
	defer func() { commitTxFn = origCommit }()
	commitTxFn = func(_ *sql.Tx) error {
		return &os.PathError{Op: "commit", Path: "loto.db", Err: syscall.EIO}
	}

	live := func(string, int, int64) bool { return true }
	now := time.Now()
	rec := domain.LockRecord{
		Target:      domain.Target{Canonical: p},
		OwnerUUID:   tcAlice,
		SessionUUID: "s1",
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}

	start := time.Now()
	_, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected commit-failure error from AcquireLocks")
	}
	if elapsed > time.Second {
		t.Errorf("AcquireLocks stalled %v on commit failure — detached audit self-contends with the still-open tx (loto-rmyg)", elapsed)
	}

	// The acquire_rollback_started breadcrumb must be durably written — it is
	// the only post-crash pointer at orphan-mode files.
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE event_kind=?`, EventAcquireRollbackStart,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("acquire_rollback_started breadcrumb dropped on commit failure (loto-rmyg): want 1, got %d", n)
	}

	// Owner-write must be restored on the stripped file.
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("write bit not restored after commit-failure rollback, got %o", st.Mode().Perm())
	}
}

// Regression for loto-1qed: DoctorRepair's post-commit restore-failure audit
// was the one restore path still riding the caller's cancellable ctx
// (restoreAndAudit -> appendModeRestoreFailedEvent -> AppendEvent(ctx)). A
// cancellation landing right after commit scaled busy_timeout to ~1ms and the
// mode_restore_failed event was silently dropped. The fix routes it through
// appendAuditDetached, matching the acquire/release/break paths.
func TestDoctorRepair_RestoreAuditSurvivesCancelledCtx(t *testing.T) {
	s := mustOpen(t)
	var stderr bytes.Buffer
	s.SetStderr(&stderr)

	dead := func(string, int, int64) bool { return false }
	l := mkFileLock(t, "dr.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{l}, func(string, int, int64) bool { return true }); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The main repair tx runs and commits on a live ctx. When the post-commit
	// restore fires (fchmod with the write bit set), cancel the caller ctx and
	// fail the restore — mirroring a Ctrl-C landing right after commit.
	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == l.Target.Canonical && mode.Perm()&0o200 != 0 {
			cancel()
			return &os.PathError{Op: opFchmod, Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	if err := s.DoctorRepair(ctx, l.Host, "doctor", dead); err != nil {
		t.Fatalf("repair should succeed (commit happened before cancel): %v", err)
	}

	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM events WHERE target_canonical=? AND event_kind='mode_restore_failed'`, l.Target.Canonical,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("mode_restore_failed audit dropped under cancelled ctx (loto-1qed): want 1, got %d", n)
	}
}
