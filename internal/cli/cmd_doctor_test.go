package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorHealthyEmpty(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "healthy") {
		t.Errorf("expected ✓ healthy: %q", out.String())
	}
}

func TestDoctorDryRunDoesNotMutate(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdDoctor, "--dry-run"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("doctor --dry-run exit %d", code)
	}
	if !strings.Contains(out.String(), "dry-run") {
		t.Errorf("expected dry-run line: %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"status", tcFlagMine}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatal("status failed")
	}
	if !strings.Contains(out.String(), "target=a.go") {
		t.Errorf("lock should still exist after --dry-run; got %q", out.String())
	}
}

func TestDoctor_OrphanModeFlaggedNotRepaired(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagRepair, tcFlagOrphan}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}

	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("orphan unexpectedly restored: %o", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("expected orphan-mode in output: %s", out.String())
	}
}

func TestDoctor_DefaultDoesNotWalkTree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}
	if strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("default doctor should not walk: %s", out.String())
	}
}

// TestDoctor_OrphanModeWalkErrorIsSurfaced verifies that filepath.WalkDir errors
// (e.g. permission-denied subtrees) are surfaced rather than silently swallowed.
// Without the fix, the doctor reports a clean scan even when files were inaccessible.
func TestDoctor_OrphanModeWalkErrorIsSurfaced(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not gate root")
	}
	repo := withTempProject(t)
	pinAgent(t)

	// Create an unreadable subdir: WalkDir will invoke the fn with a non-nil err
	// when attempting to read its children.
	denied := filepath.Join(repo, "denied")
	if err := os.Mkdir(denied, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(denied, "inside.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(denied, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0o755) })

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagOrphan}, &out, io.Discard)
	got := out.String()

	if code == 0 {
		t.Errorf("expected non-zero exit on incomplete scan, got 0: %s", got)
	}
	if !strings.Contains(got, "scan-skipped") {
		t.Errorf("expected ✗ scan-skipped line in output: %s", got)
	}
	if !strings.Contains(got, "✗") {
		t.Errorf("expected ✗ glyph: %s", got)
	}
}

func TestDoctor_RestoreOrphanModeFlagRepairs(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	code := Run([]string{tcCmdDoctor, tcFlagRepair, "--restore-orphan-mode"}, &out, io.Discard)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out.String())
	}

	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored, got %o", st.Mode().Perm())
	}
}
