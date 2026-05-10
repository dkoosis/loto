package cli

import (
	"bytes"
	"strings"
	"testing"

	"loto/internal/identity"
)

// twoAgents creates two agents in the shared HOME (simulating two sessions on
// the same host) and returns them.
func twoAgents(t *testing.T) (alice, bob *identity.Agent) {
	t.Helper()
	t.Setenv("LOTO_AGENT_ID", "")
	a, err := identity.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", "")
	b, err := identity.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	return a, b
}

func TestInboxListsUnread(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdTag, tcTargetA, "--to", bob.UUID, "ping"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("alice tag failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdInbox}, &out, &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("inbox exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "target=a.go") || !strings.Contains(out.String(), `intent="ping"`) {
		t.Errorf("expected ping tag for bob: %q", out.String())
	}
}

func TestInboxMarkReadHidesPrior(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdTag, tcTargetA, "--to", bob.UUID, "first"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("tag failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdInbox, "--unread", "--mark-read"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("first inbox failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdInbox, "--unread"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("second inbox exit %d", code)
	}
	if !strings.Contains(out.String(), "✓ no unread") {
		t.Errorf("expected no unread after mark-read; got %q", out.String())
	}
}
