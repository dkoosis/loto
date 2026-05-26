package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

func TestDoctorListsStaleLocks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAuditWith(ctx, l.Host, dead, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleLocks) != 1 {
		t.Fatalf("expected 1 stale lock, got %d", len(report.StaleLocks))
	}
}

func TestDoctorRepairReclaims(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, l.Host, "doctor-agent", dead); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("stale lock should be reclaimed, got %+v", got)
	}
}

func TestDoctorAudit_DetectsOrphanModeFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "orphan.go")
	clean := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clean, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := mustOpen(t)
	orphans, err := s.ScanOrphanModes(context.Background(), []string{orphan, clean})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != orphan {
		t.Errorf("orphans = %v, want [%s]", orphans, orphan)
	}
}

func TestScanOrphanModes_OwnedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	owned := filepath.Join(dir, "owned.go")
	if err := os.WriteFile(owned, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	s := mustOpen(t)
	ctx := context.Background()
	now := time.Now()
	l := domain.LockRecord{
		Target:      domain.Target{Canonical: owned},
		OwnerUUID:   "alice",
		SessionUUID: "alice",
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.ScanOrphanModes(ctx, []string{owned})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 0 {
		t.Errorf("owned file flagged as orphan: %v", orphans)
	}
}

func TestRestoreOrphanMode_ChmodsToWritable(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)
	restored, failures, err := s.RestoreOrphanMode(context.Background(), []string{p})
	if err != nil {
		t.Fatalf("RestoreOrphanMode: %v", err)
	}
	if len(restored) != 1 || restored[0] != p {
		t.Fatalf("restored = %v", restored)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("not writable: %o", st.Mode().Perm())
	}
}

// TestRestoreOrphanMode_HoldsOpFlock asserts RestoreOrphanMode serializes
// against the project op-flock so a concurrent Acquire can't mutate the
// lock/orphan set mid-restore (loto-98v, gh#124). If an external holder owns
// op-flock, RestoreOrphanMode must wait — verified by a short flock timeout
// causing ErrFlockTimeout rather than a torn restore.
func TestRestoreOrphanMode_HoldsOpFlock(t *testing.T) {
	t.Setenv("LOTO_FLOCK_TIMEOUT", "100ms")
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	s := mustOpen(t)

	// External holder of op-flock — simulates a concurrent AcquireLocks
	// (or any other op-flock-taking path) in flight.
	h, err := acquireOpFlock(context.Background(), s.opFlockPath(), nil)
	if err != nil {
		t.Fatalf("acquireOpFlock: %v", err)
	}

	_, _, err = s.RestoreOrphanMode(context.Background(), []string{p})
	if !errors.Is(err, ErrFlockTimeout) {
		t.Fatalf("expected ErrFlockTimeout, got %v", err)
	}
	// File must still be read-only — restore didn't proceed.
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("restore happened despite flock contention: %o", st.Mode().Perm())
	}

	h.release()

	// After release, restore succeeds.
	restored, failures, err := s.RestoreOrphanMode(context.Background(), []string{p})
	if err != nil {
		t.Fatalf("post-release RestoreOrphanMode: %v", err)
	}
	if len(restored) != 1 || len(failures) != 0 {
		t.Fatalf("post-release restored=%v failures=%v", restored, failures)
	}
}

func TestDoctorSidecarMissingDirIsNoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alive := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, alive); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAuditWith(ctx, l.Host, alive, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 1 || report.SidecarFindings[0].Reason != SidecarReasonNoSidecar {
		t.Fatalf("expected one no-sidecar finding, got %+v", report.SidecarFindings)
	}
}

func TestDoctorSidecarDisabledWhenDirEmpty(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alive := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, alive); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAuditWith(ctx, l.Host, alive, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("expected no findings when sidecar dir empty, got %+v", report.SidecarFindings)
	}
}

func TestDoctorSidecarCwdMismatch(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alive := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, alive); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"cwd":"/somewhere/else"}`, l.PID)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAuditWith(ctx, l.Host, alive, SidecarCheck{
		SidecarDir: dir,
		RepoTop:    "/Users/me/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 1 || report.SidecarFindings[0].Reason != SidecarReasonCwdMismatch {
		t.Fatalf("expected cwd-mismatch, got %+v", report.SidecarFindings)
	}
	if report.SidecarFindings[0].Detail != "/somewhere/else" {
		t.Fatalf("expected detail to carry sidecar cwd, got %q", report.SidecarFindings[0].Detail)
	}
}

func TestDoctorSidecarHealthyWhenCwdMatches(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	alive := func(string, int) bool { return true }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, alive); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repoTop := "/Users/me/repo"
	body := fmt.Sprintf(`{"pid":%d,"cwd":%q}`, l.PID, repoTop)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAuditWith(ctx, l.Host, alive, SidecarCheck{
		SidecarDir: dir,
		RepoTop:    repoTop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("expected no findings when cwd matches, got %+v", report.SidecarFindings)
	}
}

func TestDoctorSidecarSkippedForStaleLocks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAuditWith(ctx, l.Host, dead, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    "/repo",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleLocks) != 1 {
		t.Fatalf("expected stale lock, got %d", len(report.StaleLocks))
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("sidecar check should not double-report stale locks, got %+v", report.SidecarFindings)
	}
}

func TestMoveCorruptDB(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	s, _ := Open(dbPath)
	s.Close()

	moved, err := moveCorruptAside(dbPath, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if moved == "" {
		t.Fatal("expected moved path")
	}
}

// isCorruptDB must trip on real sqlite NOTADB/CORRUPT errors only — not on
// arbitrary wrapped errors that happen to contain the substring "malformed".
// Regression: gh#48 — string-match isCorruptDB destroys DB on false positives.

func TestIsCorruptDB_RealNotADatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "garbage.db")
	if err := os.WriteFile(path, []byte("not a sqlite file, just garbage bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pingErr := db.PingContext(context.Background())
	if pingErr == nil {
		t.Fatal("expected ping to fail on garbage file")
	}
	if !isCorruptDB(pingErr) {
		t.Fatalf("isCorruptDB must recognize real SQLITE_NOTADB, got: %v", pingErr)
	}
}

var (
	errSpoofMalformed = errors.New("transient network read: database disk image is malformed (cached)")
	errSpoofNotADB    = errors.New("file is not a database (from middleware)")
	errVACUUMStub     = errors.New("disk I/O error during VACUUM")
)

func TestIsCorruptDB_NotFooledBySubstring(t *testing.T) {
	// Plain wrapped errors containing corrupt-shaped substrings must NOT
	// trip corrupt detection — only real sqlite errno codes do.
	if isCorruptDB(fmt.Errorf("wrap: %w", errSpoofMalformed)) {
		t.Fatal("isCorruptDB false-positive on substring match — would destroy a healthy DB")
	}
	if isCorruptDB(errSpoofNotADB) {
		t.Fatal("isCorruptDB false-positive on substring match")
	}
}

// moveCorruptAside must be all-or-nothing: either every existing sibling
// (db, -wal, -shm) is moved aside together, or nothing moves. A concurrent
// opener must never see a fresh loto.db paired with a stale -wal.

func TestMoveCorruptAsideAtomic(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// Force WAL+SHM into existence with a write.
	if _, err := s.db.ExecContext(context.Background(), `CREATE TABLE tmp(x INTEGER)`); err != nil {
		t.Fatal(err)
	}
	s.Close()

	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		if _, err := os.Stat(dbPath + sfx); err != nil {
			// -wal/-shm may not exist after Close; that's fine. Re-create to test.
			if sfx != "" {
				_ = os.WriteFile(dbPath+sfx, []byte("sidecar"), 0o600)
			}
		}
	}

	when := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	moved, err := moveCorruptAside(dbPath, when)
	if err != nil {
		t.Fatalf("moveCorruptAside: %v", err)
	}

	// After move-aside: the original three names must all be gone together.
	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		if _, err := os.Stat(dbPath + sfx); !os.IsNotExist(err) {
			t.Errorf("expected %s removed, stat err=%v", dbPath+sfx, err)
		}
	}
	// And the move-aside artifact must hold all three.
	for _, sfx := range []string{"", sqliteWALSuffix, sqliteSHMSuffix} {
		want := filepath.Join(moved, "loto.db"+sfx)
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected %s in moved dir, stat err=%v", want, err)
		}
	}
}

func TestDoctorRepair_RestoresWriteMode(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkFileLock(t, "d.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, l.Host, "doctor", dead); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("repair must restore owner-write on reclaimed targets, got %o", st.Mode().Perm())
	}
}

func TestDoctorRepair_MultipleStaleLocksSameOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	a := mkFileLock(t, "a.go", "alice", time.Hour)
	b := mkFileLock(t, "b.go", "alice", time.Hour)
	c := mkFileLock(t, "c.go", "alice", time.Hour)
	// All three under one transaction, same actor + same now() inside reclaim
	// — the old deterministic event ID would collide. Verify all reclaim.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b, c}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, a.Host, "doctor", dead); err != nil {
		t.Fatalf("repair with multiple stale locks: %v", err)
	}
	for _, l := range []domain.LockRecord{a, b, c} {
		got, _ := s.LockAt(ctx, l.Target)
		if got != nil {
			t.Errorf("%s: stale lock should be reclaimed, got %+v", l.Target.Canonical, got)
		}
	}
}

func TestMoveCorruptAside_PreservesBytesOnCommitFailure(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")
	corruptBytes := []byte("not a real sqlite db, but unique")
	if err := os.WriteFile(dbPath, corruptBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+sqliteWALSuffix, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Pre-create the final commit destination as a non-empty directory so the
	// final os.Rename(staging, finalDir) fails with ENOTEMPTY. The defer must
	// then preserve the corrupt bytes under .corrupt.failed.<stamp>/, not
	// RemoveAll them.
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	finalDir := fmt.Sprintf("%s.corrupt.%s", dbPath, stamp.UTC().Format("2006-01-02T15-04-05Z"))
	if err := os.MkdirAll(finalDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(finalDir, "blocker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := moveCorruptAside(dbPath, stamp)
	if err == nil {
		t.Fatal("expected commit-rename failure")
	}

	// The corrupt bytes must still exist somewhere on disk — either in the
	// failed-quarantine path or in the unrenamed staging dir.
	failed := fmt.Sprintf("%s.corrupt.failed.%s", dbPath, stamp.UTC().Format("2006-01-02T15-04-05Z"))
	found := false
	for _, candidate := range []string{filepath.Join(failed, filepath.Base(dbPath))} {
		if body, err := os.ReadFile(candidate); err == nil && bytes.Equal(body, corruptBytes) {
			found = true
			break
		}
	}
	if !found {
		entries, _ := os.ReadDir(dir)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("corrupt DB bytes lost after commit-rename failure; dir contents: %v", names)
	}
}

// TestDoctorRepair_VACUUMFailureDoesNotMaskSuccess verifies that a VACUUM
// error after a successful repair transaction does not propagate as the
// DoctorRepair return value. The operator must not see "repair failed" when
// the actual repair (reclaim + chmod) succeeded. gh#127.
func TestDoctorRepair_VACUUMFailureDoesNotMaskSuccess(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	dead := func(string, int) bool { return false }
	l := mkFileLock(t, "v.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, func(string, int) bool { return true }); err != nil {
		t.Fatal(err)
	}

	// Inject a VACUUM that always fails.
	var stderr bytes.Buffer
	s.stderr = &stderr
	origVacuum := vacuumFn
	vacuumFn = func(_ context.Context, _ *sql.DB) error {
		return errVACUUMStub
	}
	t.Cleanup(func() { vacuumFn = origVacuum })

	if err := s.DoctorRepair(ctx, l.Host, "doctor", dead); err != nil {
		t.Fatalf("VACUUM failure must not surface as DoctorRepair error: %v", err)
	}

	// Lock must still be reclaimed despite VACUUM failure.
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatal("stale lock should be reclaimed even when VACUUM fails")
	}

	// VACUUM error must be logged to stderr.
	if !bytes.Contains(stderr.Bytes(), []byte("VACUUM after repair")) {
		t.Fatalf("expected VACUUM warning on stderr, got %q", stderr.String())
	}
}
