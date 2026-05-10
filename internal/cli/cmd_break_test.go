package cli

import (
	"bytes"
	"testing"
)

func TestBreakLiveRequiresForce(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{"lock", "a.go"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("lock failed")
	}
	var errBuf bytes.Buffer
	code := Run([]string{"break", "a.go", "--reason", "x"}, &bytes.Buffer{}, &errBuf)
	if code != 1 {
		t.Fatalf("expected exit 1 (live, no force); got %d; err=%q", code, errBuf.String())
	}
	code = Run([]string{"break", "a.go", "--force", "--reason", "deadline"}, &bytes.Buffer{}, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("--force should succeed; got %d", code)
	}
}
