package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	// cmdWhoamiName centralizes the subcommand name across whoami tests so
	// goconst does not flag the repeated literal (loto-u7b7).
	cmdWhoamiName = "whoami"
	flagJSON      = "--json"
)

// TestWhoamiOwnerIsTheSessionID pins the ownership contract at the surface
// the SessionStart hook reads (loto-jnid): inside a Claude Code session the
// uuid row IS the session id, and whoami leaves a session record behind for
// identity.ProbeSession.
func TestWhoamiOwnerIsTheSessionID(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnvForTest(t, "LOTO_BASE")
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")
	const sid = "cccccccc-0000-4000-8000-00000000000c"
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)

	var stdout, stderr bytes.Buffer
	if code := Run([]string{cmdWhoamiName}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"uuid:    " + sid, "session: " + sid} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("want %q in output: %q", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "handle:") {
		t.Errorf("handles are retired; output must not carry one: %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".loto", "session", sid+".json")); err != nil {
		t.Errorf("whoami must record the session for ProbeSession: %v", err)
	}
}

// TestWhoamiJSON pins the SessionStart hook contract (loto-u7b7): `loto whoami
// --json` must emit a single valid JSON object carrying the identity fields so
// the hook can `json.load(...)["uuid"]` to export LOTO_AGENT_ID.
func TestWhoamiJSON(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")
	const sid = "dddddddd-0000-4000-8000-00000000000d"
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	var stdout bytes.Buffer
	code := Run([]string{cmdWhoamiName, flagJSON}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var got struct {
		UUID    string `json:"uuid"`
		Host    string `json:"host"`
		Session string `json:"session"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, stdout.String())
	}
	if got.UUID != sid {
		t.Errorf("uuid = %q, want the session id %q", got.UUID, sid)
	}
	if got.Session != sid {
		t.Errorf("session = %q, want %q", got.Session, sid)
	}
}

// TestWhoamiUnpinnedStillAnswers: whoami is the one verb that must work from a
// bare shell. It answers on a throwaway id and says so on stderr, records no
// session (there is none), and `--ensure --json` stays an accepted no-op so
// the hook's spelling never becomes a parse error.
func TestWhoamiUnpinnedStillAnswers(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsetEnvForTest(t, "LOTO_BASE")
	unsetEnvForTest(t, "LOTO_AGENT_ID")
	unsetEnvForTest(t, "LOTO_SUBAGENT_ID")
	unsetEnvForTest(t, "CLAUDE_CODE_SESSION_ID")
	unsetEnvForTest(t, "LOTO_SESSION_ID")

	var stdout, stderr bytes.Buffer
	if code := Run([]string{cmdWhoamiName, "--ensure", flagJSON}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var got map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\noutput: %q", err, stdout.String())
	}
	if got["uuid"] == "" {
		t.Errorf("uuid empty: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "identity=unpinned") {
		t.Errorf("unpinned whoami must say so on stderr: %q", stderr.String())
	}
	if entries, _ := os.ReadDir(filepath.Join(home, ".loto", "session")); len(entries) != 0 {
		t.Errorf("no session to record on a bare shell, got %d files", len(entries))
	}
}
