package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestWhoamiPrintsHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	var stdout bytes.Buffer
	code := Run([]string{"whoami"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "handle:") {
		t.Errorf("expected handle in output: %q", stdout.String())
	}
}
