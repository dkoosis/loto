package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// cmdWhoamiName centralizes the subcommand name across whoami tests so goconst
// does not flag the repeated literal (loto-u7b7).
const cmdWhoamiName = "whoami"

func TestWhoamiPrintsHandle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	var stdout bytes.Buffer
	code := Run([]string{cmdWhoamiName}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), "handle:") {
		t.Errorf("expected handle in output: %q", stdout.String())
	}
}

// TestWhoamiJSON pins the SessionStart hook contract (loto-u7b7): `loto whoami
// --json` must emit a single valid JSON object carrying the identity fields so
// the hook can `json.load(...)["uuid"]` to export LOTO_AGENT_ID. The legacy
// human path ignored --json and printed handle:/uuid:/host:, which json.load
// could not parse, so AGENT_ID was never set.
func TestWhoamiJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	var stdout bytes.Buffer
	code := Run([]string{cmdWhoamiName, "--json"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		UUID   string `json:"uuid"`
		Handle string `json:"handle"`
		Host   string `json:"host"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, stdout.String())
	}
	if got.UUID == "" {
		t.Errorf("uuid field empty in JSON output: %q", stdout.String())
	}
	if got.Handle == "" {
		t.Errorf("handle field empty in JSON output: %q", stdout.String())
	}
}

// TestWhoamiEnsureFlag guards back-compat: the hook passes `--ensure --json`,
// so --ensure must remain an accepted no-op flag, not a parse error.
func TestWhoamiEnsureFlag(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	var stdout bytes.Buffer
	code := Run([]string{cmdWhoamiName, "--ensure", "--json"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, stdout.String())
	}
}
