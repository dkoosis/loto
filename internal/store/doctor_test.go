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

const tcRepoTop = "/repo"

func TestDoctorListsStaleLocks(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, l.Host, true, deadProbe, SidecarCheck{})
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
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, "doctor-agent", deadProbe); err != nil {
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

	s := mustOpenWithRepoTop(t, dir)
	orphans, err := s.ScanOrphanModes(context.Background(), liveProbe, []string{orphan, clean})
	if err != nil {
		t.Fatal(err)
	}
	if len(orphans) != 1 || orphans[0] != orphan {
		t.Errorf("orphans = %v, want [%s]", orphans, orphan)
	}
}

func TestScanOrphanModes_OwnedFileSkipped(t *testing.T) {
	dir := t.TempDir()
	// ‡ Do not "clean up" this chdir (loto-qoic). The lock row below is
	// repo-relative, which is the only form production can store, and
	// AcquireLocks -> validateFileTarget Lstat's Target.Canonical bare, against
	// the process CWD (locks_acquire.go). Without the chdir this test dies in
	// setup. That bare Lstat is loto-j39r's bug, noted here, not fixed here.
	t.Chdir(dir)
	owned := filepath.Join(dir, "owned.go")
	if err := os.WriteFile(owned, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	s := mustOpenWithRepoTop(t, dir)
	ctx := context.Background()
	now := time.Now()
	// ‡ Repo-relative, as domain.Canonicalize would produce. Earlier this
	// fixture stored the ABSOLUTE path — a value Canonicalize rejects and
	// production can never write — and then passed the same absolute string as
	// the candidate, so both sides agreed and the test passed while the owned-lock
	// filter was dead in production. That fiction is what hid loto-qoic. The
	// candidate stays absolute, as doctor supplies it.
	l := domain.LockRecord{
		Target:      domain.Target{Canonical: "owned.go"},
		OwnerUUID:   "alice",
		SessionUUID: "alice",
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.ScanOrphanModes(ctx, liveProbe, []string{owned})
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
	s := mustOpenWithRepoTop(t, dir)
	restored, failures, err := s.RestoreOrphanMode(context.Background(), liveProbe, []string{p})
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
	s := mustOpenWithRepoTop(t, dir)

	// External holder of op-flock — simulates a concurrent AcquireLocks
	// (or any other op-flock-taking path) in flight.
	h, err := acquireOpFlock(context.Background(), s.opFlockPath(), nil)
	if err != nil {
		t.Fatalf("acquireOpFlock: %v", err)
	}

	_, _, err = s.RestoreOrphanMode(context.Background(), liveProbe, []string{p})
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
	restored, failures, err := s.RestoreOrphanMode(context.Background(), liveProbe, []string{p})
	if err != nil {
		t.Fatalf("post-release RestoreOrphanMode: %v", err)
	}
	if len(restored) != 1 || len(failures) != 0 {
		t.Fatalf("post-release restored=%v failures=%v", restored, failures)
	}
}

// TestRestoreOrphanMode_SkipsRelockedPaths asserts that RestoreOrphanMode
// re-validates ownership under op-flock and does NOT chmod a path that became
// locked between scan and restore (loto-h85e TOCTOU). The genuine-orphan in the
// same call must still be restored so we verify per-path behaviour.
func TestRestoreOrphanMode_SkipsRelockedPaths(t *testing.T) {
	dir := t.TempDir()
	// ‡ Required, and required for the same reason as
	// TestScanOrphanModes_OwnedFileSkipped — see the note there (loto-qoic).
	t.Chdir(dir)
	// genuineOrphan: read-only, no lock row — should be restored.
	genuine := filepath.Join(dir, "genuine.go")
	if err := os.WriteFile(genuine, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	// raced: read-only on disk, but a lock row is inserted before restore runs.
	raced := filepath.Join(dir, "raced.go")
	if err := os.WriteFile(raced, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	s := mustOpenWithRepoTop(t, dir)
	ctx := context.Background()

	// Simulate the TOCTOU window: scan first (both appear as orphans at this
	// point), then acquire a lock on raced before calling RestoreOrphanMode.
	scanned := []string{genuine, raced}

	now := time.Now()
	// Repo-relative row + absolute candidate: the production pairing.
	racedLock := domain.LockRecord{
		Target:      domain.Target{Canonical: "raced.go"},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	// AcquireLocks writes back the file to writable first, then strips write for
	// KindFile — but for this test we only need the lock row in the DB. Reset
	// the file back to read-only to replicate the real scenario.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{racedLock}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(raced, 0o444); err != nil {
		t.Fatal(err)
	}

	// Now call RestoreOrphanMode with the stale scan list.
	restored, failures, err := s.RestoreOrphanMode(ctx, liveProbe, scanned)
	if err != nil {
		t.Fatalf("RestoreOrphanMode: %v", err)
	}
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}

	// genuine must be restored.
	if len(restored) != 1 || restored[0] != genuine {
		t.Errorf("restored = %v, want [%s]", restored, genuine)
	}
	st, _ := os.Stat(genuine)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("genuine orphan not writable after restore: %o", st.Mode().Perm())
	}

	// raced must NOT be restored — write bit must stay stripped.
	st2, _ := os.Stat(raced)
	if st2.Mode().Perm()&0o200 != 0 {
		t.Errorf("raced path was chmod-restored despite active lock: %o", st2.Mode().Perm())
	}
}

func TestDoctorSidecarMissingDirIsNoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, liveProbe, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    tcRepoTop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 1 || report.SidecarFindings[0].Reason != SidecarReasonNoSidecar {
		t.Fatalf("expected one no-sidecar finding, got %+v", report.SidecarFindings)
	}
}

// TestDoctorSidecarSkippedWhenHostUnknown is the loto-0yot regression: a
// caller with no verifiable host identity (hostKnown=false) must not run the
// sidecar cross-check even when the lock's recorded Host string happens to
// equal thisHost — hostKnown false means thisHost itself isn't trustworthy,
// so any equality it participates in is meaningless (mirrors liveProbe's
// !HostKnown guard from loto-u7e).
func TestDoctorSidecarSkippedWhenHostUnknown(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, false, liveProbe, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    tcRepoTop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("expected no sidecar findings when hostKnown=false, got %+v", report.SidecarFindings)
	}
}

// TestDoctorSidecarSkippedForEmptyHostLock is the exact hazard the bead
// names: a lock recorded by another hostname-broken machine has Host=="".
// This caller is also host-unknown, so thisHost=="" too — the old code
// compared "" == "" and ran the cross-check against a pid from a foreign
// kernel. hostKnown=false must block that regardless of the string match.
func TestDoctorSidecarSkippedForEmptyHostLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	l.Host = ""
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, "", false, liveProbe, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    tcRepoTop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("expected no sidecar findings for host-unknown empty-host lock, got %+v", report.SidecarFindings)
	}
}

func TestDoctorSidecarDisabledWhenDirEmpty(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, liveProbe, SidecarCheck{})
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
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	body := fmt.Sprintf(`{"pid":%d,"cwd":"/somewhere/else"}`, l.PID)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, liveProbe, SidecarCheck{
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
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	repoTop := "/Users/me/repo"
	body := fmt.Sprintf(`{"pid":%d,"cwd":%q}`, l.PID, repoTop)
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, liveProbe, SidecarCheck{
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
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, deadProbe, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    tcRepoTop,
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

// TestDoctorSidecarSkippedForNoDurablePid covers the PID-0 sentinel
// (loto-j1bo): a lock placed without LOTO_PID has no CC session sidecar keyed by
// its pid, so the zombie cross-check must skip it rather than emit a spurious
// no-cc-sidecar finding (contrast TestDoctorSidecarMissingDirIsNoOp, which
// expects exactly that finding for a real pid).
func TestDoctorSidecarSkippedForNoDurablePid(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	l.PID = 0
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, l.Host, true, liveProbe, SidecarCheck{
		SidecarDir: filepath.Join(t.TempDir(), "does-not-exist"),
		RepoTop:    tcRepoTop,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.SidecarFindings) != 0 {
		t.Fatalf("PID-0 lock must not produce a sidecar finding, got %+v", report.SidecarFindings)
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

// TestSyncDir asserts the store-local parent-dir fsync helper succeeds on a
// real directory and surfaces an error for a path that cannot be opened
// (loto-4n65, same class as loto-cq6). Durability across power-loss is not
// observable from userspace, so this covers only the open→sync→close contract;
// regression coverage for the quarantine sites comes from TestMoveCorruptDB,
// TestMoveCorruptAsideAtomic, and TestMoveCorruptAside_PreservesBytesOnCommitFailure.
func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("syncDir on missing path: want error, got nil")
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
	l := mkFileLock(t, "d.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("repair must restore owner-write on reclaimed targets, got %o", st.Mode().Perm())
	}
}

func TestDoctorRepair_ReclaimLeavesModeUntouched(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "ro.md", "alice", time.Hour)
	l.Mode = domain.ModeShared
	// Deliberately read-only file: shared acquire never strips owner-write,
	// so doctor --repair must not "restore" it either.
	if err := os.Chmod(l.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatal(err)
	}
	got, _ := s.LockAt(ctx, l.Target)
	if got != nil {
		t.Fatalf("stale shared lock should be reclaimed, got %+v", got)
	}
	// The chmod-era migration runs on every repair; a target that already
	// carries owner-write must come back untouched, not "restored" to 0o600.
	st, err := os.Stat(l.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("repair must not touch a writable target's mode: want 0644, got %o", st.Mode().Perm())
	}
}

func TestDoctorRepair_MultipleStaleLocksSameOwner(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	a := mkFileLock(t, "a.go", "alice", time.Hour)
	b := mkFileLock(t, "b.go", "alice", time.Hour)
	c := mkFileLock(t, "c.go", "alice", time.Hour)
	// All three under one transaction, same actor + same now() inside reclaim
	// — the old deterministic event ID would collide. Verify all reclaim.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b, c}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
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
	l := mkFileLock(t, "v.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
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

	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
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

// TestDoctorRepair_ReleasesOpFlockBeforeVACUUM asserts the op-flock is freed
// before vacuumFn runs, so peers aren't stalled for the whole-file rewrite
// (loto-3bl0). VACUUM is post-commit and needs SQLite's own locking, not the
// op-flock — the reclaim + chmod restore that DO need it have already run.
// The injected vacuumFn probes the op-flock with a second handle under a short
// timeout: it must acquire (flock released), not time out (flock still held).
func TestDoctorRepair_ReleasesOpFlockBeforeVACUUM(t *testing.T) {
	t.Setenv("LOTO_FLOCK_TIMEOUT", "100ms")
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, "v.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	var lockedAtVacuum bool
	origVacuum := vacuumFn
	vacuumFn = func(vctx context.Context, _ *sql.DB) error {
		// A second handle must be able to take the op-flock during VACUUM iff
		// DoctorRepair released it first. A timeout means it's still held.
		h, err := acquireOpFlock(vctx, s.opFlockPath(), nil)
		if err != nil {
			if errors.Is(err, ErrFlockTimeout) {
				lockedAtVacuum = true
				return nil
			}
			return err
		}
		h.release()
		return nil
	}
	t.Cleanup(func() { vacuumFn = origVacuum })

	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatalf("DoctorRepair: %v", err)
	}
	if lockedAtVacuum {
		t.Error("op-flock still held during VACUUM — peers stall for the whole-file rewrite")
	}
}

// TestDoctorRepair_SweepsExpiredClaims covers D3 (loto-ebkc): expired claims
// accrete unless a same-territory ClaimPrefix happens to overlap them — the
// repair tx must GC them (gcClaimsTx beside gcTagsTx), leaving live claims
// untouched.
func TestDoctorRepair_SweepsExpiredClaims(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/dead", tcAlice, -time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/live", tcBob, time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatal(err)
	}
	all, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].PathPrefix != "pkg/live" {
		t.Fatalf("repair must sweep only the expired claim, got %+v", all)
	}
}

// TestDoctorAudit_ListsExpiredClaims covers D3's audit half: the report names
// every expired claim in deterministic (prefix, owner) order; live claims
// stay out of it.
func TestDoctorAudit_ListsExpiredClaims(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	// Insert out of order to pin the sort.
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/zeta", tcBob, -time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/alpha", tcAlice, -time.Minute), nil); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaim("pkg/live", "carol", time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, "h", true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.ExpiredClaims) != 2 {
		t.Fatalf("want 2 expired claims, got %+v", report.ExpiredClaims)
	}
	if report.ExpiredClaims[0].PathPrefix != "pkg/alpha" || report.ExpiredClaims[1].PathPrefix != "pkg/zeta" {
		t.Errorf("expired claims must be (prefix, owner)-sorted, got %+v", report.ExpiredClaims)
	}
}

// --- stale candidate claim reclaim (loto-u2p7) ------------------------------

// TestDoctorRepair_ReclaimsAbandonedCandidateClaim covers AC1: a
// candidate_claims row with pid=0 and a session absent from the roster —
// modeled here by liveProbe, which (like production's pidVerdict fallback)
// answers UNKNOWN for any PID<=0 record — is named by --dry-run's audit and
// swept by --repair, and a `loto lock` on its path succeeds afterward. Aged
// 10h, well past domain.CandidateClaimReclaimGrace (30m, PR #298 review) —
// TestDoctorAudit_FreshCandidateClaimSurvivesGrace pins the fresh case this
// fixture deliberately does not exercise.
func TestDoctorRepair_ReclaimsAbandonedCandidateClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcBob, time.Hour)

	cc := domain.CandidateClaim{
		PathCanonical: l.Target.Canonical, CandidateID: tcCand1,
		OwnerUUID: tcAlice, SessionUUID: "session-absent-from-roster",
		CreatedAt: time.Now().Add(-10 * time.Hour), Host: tcHost, PID: 0,
	}
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{cc}); err != nil {
		t.Fatal(err)
	}

	// The claim blocks lock acquisition before repair (blockOnCandidateClaims).
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err == nil {
		t.Fatal("lock over an unresolved candidate claim must be refused before repair")
	} else {
		var ccce *CandidateClaimConflictError
		if !errors.As(err, &ccce) {
			t.Fatalf("want *CandidateClaimConflictError, got %T: %v", err, err)
		}
	}

	report, err := s.DoctorAudit(ctx, tcHost, true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleCandidateClaims) != 1 {
		t.Fatalf("want would_gc_candidate_claims=1, got %d: %+v", len(report.StaleCandidateClaims), report.StaleCandidateClaims)
	}

	if err := s.DoctorRepair(ctx, "doctor-agent", liveProbe); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatalf("repair must remove the abandoned candidate claim, got %+v", remaining)
	}

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatalf("lock must succeed once the candidate claim is reclaimed: %v", err)
	}
}

// TestDoctorAudit_FreshCandidateClaimSurvivesGrace is the PR #298 review fix's
// integration pin: a zero-evidence candidate claim (pid=0, probe UNKNOWN) that
// is freshly minted — the documented shape for a degraded-mode submitter with
// no durable LOTO_PID — must NOT be named by --dry-run or swept by --repair.
// Without domain.CandidateClaimReclaimGrace this fixture reclaims exactly like
// TestDoctorRepair_ReclaimsAbandonedCandidateClaim's 10h-old one; the only
// difference between the two is age.
func TestDoctorAudit_FreshCandidateClaimSurvivesGrace(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcBob, time.Hour)

	cc := domain.CandidateClaim{
		PathCanonical: l.Target.Canonical, CandidateID: tcCand1,
		OwnerUUID: tcAlice, SessionUUID: "session-absent-from-roster",
		CreatedAt: time.Now(), Host: tcHost, PID: 0,
	}
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{cc}); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, tcHost, true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleCandidateClaims) != 0 {
		t.Fatalf("a fresh zero-evidence candidate claim must not be reported stale (grace floor), got %+v", report.StaleCandidateClaims)
	}

	if err := s.DoctorRepair(ctx, "doctor-agent", liveProbe); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("repair must not remove a fresh candidate claim still inside the grace floor, got %d remaining: %+v", len(remaining), remaining)
	}
}

// TestDoctorRepair_LiveSessionCandidateClaimSurvives is AC3's negative case: a
// candidate claim minted with no durable pid but owned by a session the
// probe positively confirms LIVE must not be reported stale or reclaimed —
// broadening past CandidateClaimIsDead must not also broaden past a real
// liveness witness.
func TestDoctorRepair_LiveSessionCandidateClaimSurvives(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLock(t, tcAGo, tcBob, time.Hour)

	const liveSession = domain.SessionUUID("live-session")
	sessionAlive := func(l domain.LockRecord) domain.Liveness {
		if l.SessionUUID == liveSession {
			return domain.LivenessAlive
		}
		return domain.LivenessUnknown
	}

	cc := domain.CandidateClaim{
		PathCanonical: l.Target.Canonical, CandidateID: tcCand1,
		OwnerUUID: tcAlice, SessionUUID: liveSession,
		CreatedAt: time.Now(), Host: tcHost, PID: 0,
	}
	if err := s.insertCandidateClaimsUnguarded(ctx, []domain.CandidateClaim{cc}); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, tcHost, true, sessionAlive, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleCandidateClaims) != 0 {
		t.Fatalf("a live-session candidate claim must not be reported stale, got %+v", report.StaleCandidateClaims)
	}

	if err := s.DoctorRepair(ctx, "doctor-agent", sessionAlive); err != nil {
		t.Fatal(err)
	}
	remaining, err := s.ListCandidateClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 {
		t.Fatalf("repair must not remove a live-session candidate claim, got %d remaining: %+v", len(remaining), remaining)
	}

	// Still-standing proof, not just a row count: the claim must still block.
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, sessionAlive); err == nil {
		t.Fatal("a live candidate claim must still block lock acquisition")
	} else {
		var ccce *CandidateClaimConflictError
		if !errors.As(err, &ccce) {
			t.Fatalf("want *CandidateClaimConflictError, got %T: %v", err, err)
		}
	}
}

// TestDoctorRepair_ChmodEraMigration is loto-zssw's migration leg. Until 2026-08
// acquire stripped write bits and release restored them, so a session that died
// mid-hold left files at 0444 with nothing to explain them. `doctor --repair`
// undoes that for every path loto itself locked — the current rows plus the
// retained audit trail — and touches nothing else.
func TestDoctorRepair_ChmodEraMigration(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// held: a LIVE row whose file is read-only. The migration must leave it
	// alone — a live holder may have made it read-only itself (loto-2hjh).
	held := mkFileLock(t, "held.go", "alice", time.Hour)
	// released: locked then released, so only the audit trail names it.
	released := mkFileLock(t, "released.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{held, released}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{released.Target}, "alice", liveProbe); err != nil {
		t.Fatal(err)
	}
	// stranger: never known to loto, and deliberately read-only. Must be left
	// alone — the migration is bounded by rows loto keeps, not a repo walk.
	stranger := filepath.Join(t.TempDir(), "stranger.go")
	if err := os.WriteFile(stranger, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{held.Target.Canonical, released.Target.Canonical} {
		if err := os.Chmod(p, 0o444); err != nil {
			t.Fatal(err)
		}
	}

	if err := s.DoctorRepair(ctx, "doctor", liveProbe); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(released.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("%s: chmod-era straggler not restored, perm=%o", released.Target.Canonical, st.Mode().Perm())
	}
	st, err = os.Stat(held.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Errorf("a live-locked path must keep its mode, perm=%o", st.Mode().Perm())
	}
	st, err = os.Stat(stranger)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Errorf("a path loto never locked must be left alone, perm=%o", st.Mode().Perm())
	}
}

// TestDoctorRepair_ChmodEraMigrationIsIdempotent pins the drain: once every
// straggler carries owner-write, a second repair finds nothing to do and emits
// no audit noise. The pass costs a stat per candidate and nothing more.
func TestDoctorRepair_ChmodEraMigrationIsIdempotent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	// deadProbe below makes this row stale, so it is a chmod-era candidate. A
	// LIVE row would not be one at all (loto-2hjh) and the drain would be
	// unmeasurable here.
	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}

	calls := 0
	orig := fchmodFn
	defer func() { fchmodFn = orig }()
	fchmodFn = func(f *os.File, mode os.FileMode) error {
		if f.Name() == l.Target.Canonical {
			calls++
		}
		return orig(f, mode)
	}

	for i := range 2 {
		if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
			t.Fatalf("repair %d: %v", i, err)
		}
	}
	if calls != 1 {
		t.Errorf("migration must fire once and then drain, got %d chmod calls", calls)
	}
}

// TestScanOrphanModes_AbsoluteCandidatesRequireRepoTop pins D3's guard. Both
// arms are bases that would silently reproduce loto-qoic: an empty repoTop
// cannot join a repo-relative row up to an absolute candidate, and a relative
// repoTop joins to something that can never equal one. A wrong-but-absolute
// repoTop stays unguarded by design — the store cannot validate a base it does
// not own (loto-6e02).
func TestScanOrphanModes_AbsoluteCandidatesRequireRepoTop(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	abs := filepath.Join(dir, "a.go")
	if err := os.WriteFile(abs, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	for _, tc := range []struct {
		name    string
		repoTop string
	}{
		{"empty repoTop with an absolute candidate", ""},
		{"relative repoTop", "some/rel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The base is now the store's own (loto-6e02), so the bad base is
			// injected at open rather than passed per call.
			bad := mustOpenWithRepoTop(t, tc.repoTop)
			orphans, err := bad.ScanOrphanModes(ctx, liveProbe, []string{abs})
			if !errors.Is(err, errOrphanNoRepoTop) {
				t.Errorf("err = %v, want errOrphanNoRepoTop", err)
			}
			if orphans != nil {
				t.Errorf("orphans = %v, want nil on refusal", orphans)
			}
		})
	}
}

// TestScanOrphanModes_ExpiredLockRowDoesNotSuppress pins D8. Only LIVE rows
// suppress an orphan report. A file left read-only by a session that died
// mid-hold is exactly what orphan mode exists to surface, so an expired row
// must not silence it — the row survives in `locks` until --repair reclaims it.
func TestScanOrphanModes_ExpiredLockRowDoesNotSuppress(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	stale := filepath.Join(dir, "stale.go")
	if err := os.WriteFile(stale, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	s := mustOpenWithRepoTop(t, dir)
	ctx := context.Background()
	now := time.Now()
	l := domain.LockRecord{
		Target:      domain.Target{Canonical: "stale.go"},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		Intent:      tcTest,
		CreatedAt:   now.Add(-2 * time.Hour),
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Expire the lease behind the acquire path, which refuses to mint one that
	// is already dead. This is the state a session that died mid-hold leaves.
	if _, err := s.db.ExecContext(ctx,
		`UPDATE locks SET expires_at = ? WHERE target_canonical = ?`,
		now.Add(-time.Minute).UnixNano(), "stale.go"); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o444); err != nil {
		t.Fatal(err)
	}

	orphans, err := s.ScanOrphanModes(ctx, liveProbe, []string{stale})
	if err != nil {
		t.Fatalf("ScanOrphanModes: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != stale {
		t.Errorf("orphans = %v, want [%s] — an expired row must not suppress", orphans, stale)
	}
}

// TestScanOrphanModes_DeadHolderBeforeTTLDoesNotSuppress pins the crash case
// orphan mode exists for (loto-j863). A holder SIGKILLed before its TTL lapses
// leaves a read-only file behind and a lock row that has NOT expired. Filtering
// liveness on expiry alone would suppress exactly that file — a false negative
// in the one scenario orphan recovery is meant to surface. The owned set must
// use domain.EvalContext.IsStale, the same predicate reclaimStaleLocks applies.
func TestScanOrphanModes_DeadHolderBeforeTTLDoesNotSuppress(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	crashed := filepath.Join(dir, "crashed.go")
	if err := os.WriteFile(crashed, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	s := mustOpenWithRepoTop(t, dir)
	ctx := context.Background()
	now := time.Now()
	// Lease still an hour from lapsing — only the holder is gone.
	l := domain.LockRecord{
		Target:      domain.Target{Canonical: "crashed.go"},
		OwnerUUID:   tcAlice,
		SessionUUID: tcAlice,
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        tcHost,
		PID:         1,
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(crashed, 0o444); err != nil {
		t.Fatal(err)
	}

	// deadProbe: every local pid gone. TTL has NOT lapsed, so an expiry-only
	// filter would still call this row live and swallow the report.
	orphans, err := s.ScanOrphanModes(ctx, deadProbe, []string{crashed})
	if err != nil {
		t.Fatalf("ScanOrphanModes: %v", err)
	}
	if len(orphans) != 1 || orphans[0] != crashed {
		t.Errorf("orphans = %v, want [%s] — a dead holder inside its TTL must not suppress", orphans, crashed)
	}

	// And the live-holder case still suppresses, so this is a liveness test and
	// not just "the filter is off".
	orphans, err = s.ScanOrphanModes(ctx, liveProbe, []string{crashed})
	if err != nil {
		t.Fatalf("ScanOrphanModes (live): %v", err)
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v, want none — a live holder must still suppress", orphans)
	}
}

// TestDoctorRepair_LiveLockKeepsModeUntilReleased is the loto-2hjh regression.
// The north-star harm is loto undoing its own lock's protection: `doctor
// --repair`, with no orphan flag, used to add owner-write to a file loto held a
// live lock on. The same file must be restored once the lock is gone — the
// migration's actual job is unchanged, only its warrant is narrower.
func TestDoctorRepair_LiveLockKeepsModeUntilReleased(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	l := mkFileLock(t, "a.go", "alice", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(l.Target.Canonical, 0o444); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, "doctor", liveProbe); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(l.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Fatalf("repair must not touch a live-locked path, perm=%o", st.Mode().Perm())
	}
	if got, _ := s.LockAt(ctx, l.Target); got == nil {
		t.Fatal("live lock must survive repair")
	}

	if _, err := s.ReleaseLocks(ctx, []domain.Target{l.Target}, "alice", liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.DoctorRepair(ctx, "doctor", liveProbe); err != nil {
		t.Fatal(err)
	}
	st, err = os.Stat(l.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("once released, chmod-era residue must still be restored, perm=%o", st.Mode().Perm())
	}
}

// TestRestoreChmodEraFiles_ResolvesAgainstRepoTop is the loto-gc82 regression.
// Candidates are repo-relative; resolving them bare deref'd against the process
// CWD, so `doctor --repair` from a subdirectory chmod'd a same-named write-less
// file that merely shared a base name with a lock or event row.
func TestRestoreChmodEraFiles_ResolvesAgainstRepoTop(t *testing.T) {
	repoTop := t.TempDir()
	target := filepath.Join(repoTop, "victim.go")
	if err := os.WriteFile(target, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	// The decoy sits under the caller's cwd, shares the base name, and is
	// write-less — the exact shape the old code chmod'd by mistake.
	sub := t.TempDir()
	decoy := filepath.Join(sub, "victim.go")
	if err := os.WriteFile(decoy, []byte("y"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	if ev := restoreChmodEraFiles(repoTop, []string{"victim.go"}, "doctor", time.Now()); len(ev) != 0 {
		t.Fatalf("unexpected failure events: %+v", ev)
	}

	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("candidate under repoTop must be restored, perm=%o", st.Mode().Perm())
	}
	st, err = os.Stat(decoy)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Errorf("a same-named file under cwd must be left alone, perm=%o", st.Mode().Perm())
	}
}

// TestRestoreChmodEraFiles_RelativeCandidateWithoutRepoTopIsSkipped pins the
// other half of loto-gc82: with no repo to resolve against, the only available
// guess is the process CWD, so the migration must skip rather than guess.
func TestRestoreChmodEraFiles_RelativeCandidateWithoutRepoTopIsSkipped(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "orphan.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if ev := restoreChmodEraFiles("", []string{"orphan.go"}, "doctor", time.Now()); len(ev) != 0 {
		t.Fatalf("unexpected failure events: %+v", ev)
	}
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o444 {
		t.Errorf("relative candidate with no repoTop must be skipped, perm=%o", st.Mode().Perm())
	}
}
