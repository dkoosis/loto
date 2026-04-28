//go:build unix

package loto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
	writeRawTag(t, l, "stale.go", Tag{
		AgentID: "dead-agent",
		Target:  "stale.go",
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

// TestDoctorRepairStaleTag: --repair removes the stale tag.
func TestDoctorRepairStaleTag(t *testing.T) {
	l := newTestLOTO(t)

	tagPath := writeRawTag(t, l, "stale.go", Tag{
		AgentID: "dead-agent",
		Target:  "stale.go",
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

// TestDoctorDryRun: --dry-run reports WouldRepair but does not remove tag.
func TestDoctorDryRun(t *testing.T) {
	l := newTestLOTO(t)

	tagPath := writeRawTag(t, l, "stale.go", Tag{
		AgentID: "dead-agent",
		Target:  "stale.go",
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
