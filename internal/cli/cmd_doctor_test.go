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
	code := Run([]string{tcCmdDoctor, tcFlagRepair, "--orphan-mode"}, &out, io.Discard)
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
