package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestStatusEmpty(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{"status"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"project:", "repo:", "state:", "no locks"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in: %q", want, out.String())
		}
	}
}

func TestStatusMineFilters(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{"lock", "a.go"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var out bytes.Buffer
	code := Run([]string{"status", "--mine"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "target=a.go") {
		t.Errorf("expected own lock listed: %q", out.String())
	}
}

func TestStatusSingleTargetFree(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out bytes.Buffer
	code := Run([]string{"status", "a.go"}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out.String(), "✓ free") {
		t.Errorf("expected ✓ free: %q", out.String())
	}
}
