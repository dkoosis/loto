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

// TestAcquireLocks_CommitFailureIsFastAndLeavesNothing pins loto-rmyg: a
// commit failure must return promptly and leave no residue. The original bug
// was self-contention — the failing tx stayed open (its rollback is deferred)
// while a detached audit opened a SECOND write tx against the held lock,
// stalling ~2s on busy_timeout. Since loto-zssw retired the write-strip there
// is no rollback audit at all, and the only thing left to guard is that the
// call does not stall and writes nothing.
func TestAcquireLocks_CommitFailureIsFastAndLeavesNothing(t *testing.T) {
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
	_, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, liveProbe)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected commit-failure error from AcquireLocks")
	}
	if elapsed > time.Second {
		t.Errorf("AcquireLocks stalled %v on commit failure — detached audit self-contends with the still-open tx (loto-rmyg)", elapsed)
	}

	// No row landed, and — since loto-zssw retired the write-strip — no file
	// mode was touched either, so a failed acquire leaves nothing behind to
	// clean up.
	if got, err := s.LockAt(context.Background(), rec.Target); err != nil || got != nil {
		t.Errorf("commit failure must leave no lock row, got %+v (err %v)", got, err)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Errorf("acquire must not touch mode bits, got %o", st.Mode().Perm())
	}
}

// TestDoctorRepair_MigrationAuditSurvivesCancelledCtx pins loto-1qed against
// the one chmod path that outlived the strip: doctor's chmod-era migration
// (loto-zssw). The migration runs post-commit, so a cancellation landing right
// after commit used to scale busy_timeout to ~1ms and drop the
// mode_restore_failed event silently. It must route through
// appendAuditDetached, which carries its own bounded ctx.
func TestDoctorRepair_MigrationAuditSurvivesCancelledCtx(t *testing.T) {
	s := mustOpen(t)
	var stderr bytes.Buffer
	s.setStderr(&stderr)

	// A file left read-only by the pre-zssw loto, with a lock row naming it —
	// exactly the wild state the migration exists to repair.
	l := mkFileLock(t, "dr.go", tcAlice, time.Hour)
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == l.Target.Canonical && mode.Perm()&0o200 != 0 {
			cancel()
			return &os.PathError{Op: "fchmod", Path: f.Name(), Err: syscall.EPERM}
		}
		return orig(f, mode)
	}

	if err := s.DoctorRepair(ctx, "doctor", "", deadProbe); err != nil {
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
