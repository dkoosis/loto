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
	if code := Run([]string{"tag", target, "ping"}, &out, &errBuf); code != 0 {
		t.Fatalf("tag exit=%d err=%q", code, errBuf.String())
	}
	// "✓ tag id=t-deadbeef target=…"
	s := out.String()
	const marker = "id="
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("no id= in tag output: %q", s)
	}
	rest := s[i+len(marker):]
	j := strings.Index(rest, " ")
	if j < 0 {
		t.Fatalf("malformed tag output: %q", s)
	}
	return rest[:j]
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
	if code := Run([]string{"ack", id}, &out, &errBuf); code != 0 {
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
	must0(t, []string{"ack", id})
	must0(t, []string{"ack", id}) // second ack → no-op exit 0
}

func TestCmdAck_UnknownID_NoOp(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	must0(t, []string{"ack", "t-deadbeef"})
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
	code := Run([]string{"ack", id}, &out, &errBuf)
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
