package cli

import (
	"bytes"
	"strings"
	"testing"
)

// runTagGetID locks as alice then tags as bob, returning the new tag id.
// Caller is responsible for setting LOTO_AGENT_ID before/after.
func runTagGetID(t *testing.T, target string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdTag, target, "ping"}, &out, &errBuf); code != 0 {
		t.Fatalf("tag exit=%d err=%q", code, errBuf.String())
	}
	return extractTagID(t, out.String())
}

// extractTagID parses "✓ tag id=t-deadbeef target=…" and returns "t-deadbeef".
func extractTagID(t *testing.T, s string) string {
	t.Helper()
	_, after, ok := strings.Cut(s, "id=")
	if !ok {
		t.Fatalf("no id= in tag output: %q", s)
	}
	id, _, ok := strings.Cut(after, " ")
	if !ok {
		t.Fatalf("malformed tag output: %q", s)
	}
	return id
}

func TestCmdAck_Dismisses(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	id := runTagGetID(t, tcTargetA)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdAck, id}, &out, &errBuf); code != 0 {
		t.Fatalf("ack exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ ack id="+id) {
		t.Fatalf("expected ✓ ack id=%s in output: %q", id, out.String())
	}
}

func TestCmdAck_Idempotent(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	id := runTagGetID(t, tcTargetA)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdAck, id})
	must0(t, []string{tcCmdAck, id}) // second ack → no-op exit 0
}

func TestCmdAck_UnknownID_NoOp(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	must0(t, []string{tcCmdAck, "t-deadbeef"})
}

func TestCmdAck_NotMine_Rejects(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	id := runTagGetID(t, tcTargetA)
	// Bob (the tagger) tries to ack — addressed to alice (the lock holder).
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdAck, id}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("expected exit 3 for wrong recipient; got %d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "not addressed to you") {
		t.Fatalf("expected 'not addressed to you'; err=%q", errBuf.String())
	}
}

func TestCmdAck_UsageOnBadArgs(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{"ack"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("expected exit 2; got %d", code)
	}
}
