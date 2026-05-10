//go:build unix

package loto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	deadAgent  = "dead-agent"
	staleGoTgt = "stale.go"
)

// writeRawTag writes a Tag directly into the files/ dir under l, bypassing the
// flock — used to simulate crash remnants without holding the lock.
func writeRawTag(t *testing.T, l *LOTO, target string, tag Tag) string {
	t.Helper()
	lockPath, tagPath, err := l.filePaths(target)
	if err != nil {
		t.Fatalf("filePaths: %v", err)
	}
	// Touch the lock file so examineTagPair can open it.
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("touch lock: %v", err)
	}
	f.Close()
	data, err := json.Marshal(tag)
	if err != nil {
		t.Fatalf("marshal tag: %v", err)
	}
	if err := os.WriteFile(tagPath, data, 0o600); err != nil {
		t.Fatalf("write tag: %v", err)
	}
	return tagPath
}

// TestDoctorClean: empty base dir → clean report.
func TestDoctorClean(t *testing.T) {
	l := newTestLOTO(t)
	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !report.Clean {
		t.Fatalf("expected clean report, got findings: %+v", report.Findings)
	}
}

// TestDoctorStaleTag: a tag exists but the flock is unheld → stale_tag.
func TestDoctorStaleTag(t *testing.T) {
	l := newTestLOTO(t)

	// Write a tag with a dead-ish PID (process 0 never exists on user side).
	writeRawTag(t, l, staleGoTgt, Tag{
		AgentID: deadAgent,
		Target:  staleGoTgt,
		PID:     0, // not a real pid; flock is free
	})

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.Clean {
		t.Fatal("expected dirty report")
	}
	if len(report.Findings) != 1 || report.Findings[0].Class != DriftStaleTag {
		t.Fatalf("expected stale_tag finding, got %+v", report.Findings)
	}
}

// TestDoctorAcquiredHoldNotClassifiedAsStale: a record-tier acquire'd hold
// (tag with non-zero, unexpired ExpiresAt; flock free; PID may be dead)
// must NOT be reported as DriftStaleTag and must NOT be reaped under
// --repair. North-star carve-out: TTL is authoritative for record-tier.
func TestDoctorAcquiredHoldNotClassifiedAsStale(t *testing.T) {
	l := newTestLOTO(t)

	// AcquirePath is used against real paths; create the target file
	// so the orphan check doesn't false-positive.
	target := filepath.Join(t.TempDir(), "live.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Tag mimics what AcquirePath writes: future ExpiresAt, dead PID
	// (PID 0 since the holder process is no longer with us).
	tagPath := writeRawTag(t, l, target, Tag{
		AgentID:   deadAgent,
		Target:    target,
		PID:       0,
		Timestamp: time.Now().UTC(),
		ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	})

	// Doctor (check) must report clean.
	rep, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !rep.Clean {
		t.Fatalf("expected clean report for record-tier hold; got %+v", rep.Findings)
	}

	// Doctor --repair must not remove the tag (it is authoritative for TTL).
	if _, err := l.Doctor("test-agent", DoctorRepair); err != nil {
		t.Fatalf("Doctor repair: %v", err)
	}
	if _, err := os.Stat(tagPath); err != nil {
		t.Fatalf("tag was removed by doctor repair: %v", err)
	}
}

// TestDoctorAcquiredHoldExpiredIsStale: once the record-tier TTL elapses,
// the tag becomes a normal stale_tag and doctor should reap it.
func TestDoctorAcquiredHoldExpiredIsStale(t *testing.T) {
	l := newTestLOTO(t)

	target := filepath.Join(t.TempDir(), "expired.go")
	if err := os.WriteFile(target, []byte("package x\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tagPath := writeRawTag(t, l, target, Tag{
		AgentID:   deadAgent,
		Target:    target,
		PID:       0,
		Timestamp: time.Now().Add(-time.Hour).UTC(),
		ExpiresAt: time.Now().Add(-time.Minute).UTC(), // expired
	})

	rep, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if rep.Clean || len(rep.Findings) != 1 || rep.Findings[0].Class != DriftStaleTag {
		t.Fatalf("expected stale_tag finding for expired record-tier; got %+v", rep.Findings)
	}
	_ = tagPath
}

// TestDoctorRepairStaleTag: --repair removes the stale tag.
func TestDoctorRepairStaleTag(t *testing.T) {
	l := newTestLOTO(t)

	tagPath := writeRawTag(t, l, staleGoTgt, Tag{
		AgentID: deadAgent,
		Target:  staleGoTgt,
		PID:     0,
	})

	report, err := l.Doctor("test-agent", DoctorRepair)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(report.Findings) != 1 || report.Findings[0].Class != DriftStaleTag {
		t.Fatalf("unexpected findings: %+v", report.Findings)
	}
	if !report.Findings[0].Repaired {
		t.Error("expected Repaired=true")
	}
	if _, err := os.Stat(tagPath); !os.IsNotExist(err) {
		t.Error("expected tag file to be removed after repair")
	}

	// Second run should be clean.
	r2, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("second Doctor: %v", err)
	}
	if !r2.Clean {
		t.Fatalf("expected clean after repair, got %+v", r2.Findings)
	}
}

// TestDoctorRepairFailsPopulatesError: when --repair attempts a removal that
// fails, the Finding records the error in Finding.Error and Repaired stays false.
// Simulated by making the files/ dir read-only so os.Remove(tag) fails.
func TestDoctorRepairFailsPopulatesError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir does not block root")
	}
	l := newTestLOTO(t)

	tagPath := writeRawTag(t, l, staleGoTgt, Tag{
		AgentID: deadAgent,
		Target:  staleGoTgt,
		PID:     0,
	})

	filesDir := filepath.Dir(tagPath)
	if err := os.Chmod(filesDir, 0o500); err != nil {
		t.Fatalf("chmod files dir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(filesDir, 0o700) })

	report, err := l.Doctor("test-agent", DoctorRepair)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %+v", report.Findings)
	}
	f := report.Findings[0]
	if f.Repaired {
		t.Error("expected Repaired=false on failed repair")
	}
	if f.Error == "" {
		t.Errorf("expected Error to be populated; got empty")
	}
}

// TestDoctorDryRun: --dry-run reports WouldRepair but does not remove tag.
func TestDoctorDryRun(t *testing.T) {
	l := newTestLOTO(t)

	tagPath := writeRawTag(t, l, staleGoTgt, Tag{
		AgentID: deadAgent,
		Target:  staleGoTgt,
		PID:     0,
	})

	report, err := l.Doctor("test-agent", DoctorDryRun)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if len(report.Findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(report.Findings))
	}
	f := report.Findings[0]
	if f.Repaired {
		t.Error("expected Repaired=false in dry-run mode")
	}
	if !f.WouldRepair {
		t.Error("expected WouldRepair=true in dry-run mode")
	}
	if _, err := os.Stat(tagPath); err != nil {
		t.Error("tag file should still exist after dry-run")
	}
}

// TestDoctorOrphanedTag: .tag without .lock → orphaned finding.
func TestDoctorOrphanedTag(t *testing.T) {
	l := newTestLOTO(t)

	// Write a tag file with no matching lock.
	filesDir := filepath.Join(l.baseDir, "files")
	tagPath := filepath.Join(filesDir, "deadbeef.tag")
	tag := Tag{AgentID: "ghost", Target: "x.go", PID: 0}
	data, _ := json.Marshal(tag)
	if err := os.WriteFile(tagPath, data, 0o600); err != nil {
		t.Fatalf("write orphan tag: %v", err)
	}

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.Clean {
		t.Fatal("expected dirty report for orphaned tag")
	}
	found := false
	for _, f := range report.Findings {
		if f.Class == DriftOrphaned {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected orphaned finding, got %+v", report.Findings)
	}
}

// TestDoctorLayoutDrift: unexpected file in base dir → layout_drift.
func TestDoctorLayoutDrift(t *testing.T) {
	l := newTestLOTO(t)

	unexpected := filepath.Join(l.baseDir, "mystery.txt")
	if err := os.WriteFile(unexpected, []byte("?"), 0o600); err != nil {
		t.Fatalf("write unexpected: %v", err)
	}

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.Clean {
		t.Fatal("expected dirty report for layout drift")
	}
	found := false
	for _, f := range report.Findings {
		if f.Class == DriftLayoutDrift {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected layout_drift finding, got %+v", report.Findings)
	}
}

// TestDoctorSoftStaleHeld: lock held, PID alive, soft TTL expired → soft_stale_held.
func TestDoctorSoftStaleHeld(t *testing.T) {
	l := newTestLOTO(t)

	// Acquire with a tiny TTL, then let it expire while holding the lock.
	lock, err := l.TryFileLock("agent-a", "test", "ttl.go", TagOptions{TTL: time.Millisecond})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Unlock()

	time.Sleep(5 * time.Millisecond) // let TTL expire

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if report.Clean {
		t.Fatal("expected dirty report for soft_stale_held")
	}
	found := false
	for _, f := range report.Findings {
		if f.Class == DriftSoftStaleHeld {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected soft_stale_held finding, got %+v", report.Findings)
	}

	// soft_stale_held is report-only: repair flag should not set Repaired.
	report2, err := l.Doctor("test-agent", DoctorRepair)
	if err != nil {
		t.Fatalf("Doctor repair: %v", err)
	}
	for _, f := range report2.Findings {
		if f.Class == DriftSoftStaleHeld && f.Repaired {
			t.Error("soft_stale_held should never be marked Repaired")
		}
	}
}

// backdateTagTimestamp rewrites the on-disk tag for target with a stale timestamp
// while the caller still holds the flock. Used to simulate zombie staleness.
func backdateTagTimestamp(t *testing.T, l *LOTO, target string, ts time.Time) {
	t.Helper()
	_, tagPath, err := l.filePaths(target)
	if err != nil {
		t.Fatalf("filePaths: %v", err)
	}
	data, err := os.ReadFile(tagPath)
	if err != nil {
		t.Fatalf("read tag: %v", err)
	}
	var tag Tag
	if err := json.Unmarshal(data, &tag); err != nil {
		t.Fatalf("unmarshal tag: %v", err)
	}
	tag.Timestamp = ts
	out, err := json.Marshal(tag)
	if err != nil {
		t.Fatalf("marshal tag: %v", err)
	}
	if err := os.WriteFile(tagPath, out, 0o600); err != nil {
		t.Fatalf("write tag: %v", err)
	}
}

// TestDoctorZombieHeld: held lock + live PID + last activity older than threshold → zombie_held.
func TestDoctorZombieHeld(t *testing.T) {
	l := newTestLOTO(t)
	l.ZombieIdle = 10 * time.Millisecond

	lock, err := l.TryFileLock("agent-a", "edit", "z.go")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Unlock()

	backdateTagTimestamp(t, l, "z.go", time.Now().Add(-time.Hour))

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	found := false
	for _, f := range report.Findings {
		if f.Class == DriftZombieHeld {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected zombie_held finding, got %+v", report.Findings)
	}
}

// TestDoctorZombieHeld_RecentMsgsResetsActivity: stale tag timestamp but a
// fresh mailbox write still counts as activity → no zombie.
func TestDoctorZombieHeld_RecentMsgsResetsActivity(t *testing.T) {
	l := newTestLOTO(t)
	l.ZombieIdle = time.Hour // generous so only the backdated tag could trip it

	lock, err := l.TryFileLock("agent-a", "edit", "active.go")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Unlock()

	// Tag is acquired-now (would be fresh), but force it past the threshold.
	backdateTagTimestamp(t, l, "active.go", time.Now().Add(-2*time.Hour))

	// Recent mailbox write — bumps msgs file mtime to "now", which lastActivity
	// must observe, suppressing the zombie diagnosis even though the tag is stale.
	if err := l.SendMsg("active.go", "agent-a", "agent-b", "still here", false); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	for _, f := range report.Findings {
		if f.Class == DriftZombieHeld {
			t.Fatalf("recent mailbox write should suppress zombie_held, got %+v", report.Findings)
		}
	}
}

// TestDoctorZombieHeld_DryRun: zombie finding under --dry-run sets WouldRepair, never Repaired.
func TestDoctorZombieHeld_DryRun(t *testing.T) {
	l := newTestLOTO(t)
	l.ZombieIdle = 10 * time.Millisecond

	lock, err := l.TryFileLock("agent-a", "edit", "dry.go")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer lock.Unlock()

	backdateTagTimestamp(t, l, "dry.go", time.Now().Add(-time.Hour))

	report, err := l.Doctor("test-agent", DoctorDryRun)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	var fi *Finding
	for i := range report.Findings {
		if report.Findings[i].Class == DriftZombieHeld {
			fi = &report.Findings[i]
		}
	}
	if fi == nil {
		t.Fatalf("expected zombie_held finding, got %+v", report.Findings)
	}
	if fi.Repaired {
		t.Error("Repaired must be false in dry-run mode")
	}
	if !fi.WouldRepair {
		t.Error("WouldRepair must be true in dry-run mode")
	}
}

// TestDoctorCleanAfterRelease: normal lock/release leaves base clean.
func TestDoctorCleanAfterRelease(t *testing.T) {
	l := newTestLOTO(t)

	lock, err := l.TryFileLock("agent-a", "test", "clean.go")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if err := lock.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	report, err := l.Doctor("test-agent", DoctorCheck)
	if err != nil {
		t.Fatalf("Doctor: %v", err)
	}
	if !report.Clean {
		t.Fatalf("expected clean after proper release, got %+v", report.Findings)
	}
}
