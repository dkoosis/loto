package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestWhoamiPrintsHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOTO_AGENT_ID", "")
	var stdout bytes.Buffer
	code := Run([]string{"whoami"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "handle:") {
		t.Errorf("expected handle in output: %q", stdout.String())
	}
}
