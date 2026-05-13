package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestDoctorHealthyEmpty(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{"doctor"}, &out, &bytes.Buffer{})
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
	if code := Run([]string{"doctor", "--dry-run"}, &out, &bytes.Buffer{}); code != 0 {
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
