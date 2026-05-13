package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestMsgSendAndRead(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out bytes.Buffer
	if code := Run([]string{tcCmdMsg, bob.UUID, "-t", "hello from alice"}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("send exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "✓ sent") {
		t.Errorf("expected ✓ sent: %q", out.String())
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	out.Reset()
	if code := Run([]string{tcCmdMsg}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("read exit %d: %q", code, out.String())
	}
	if !strings.Contains(out.String(), "hello from alice") {
		t.Errorf("expected message body: %q", out.String())
	}
}

func TestMsgMarkReadHidesPrior(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdMsg, bob.UUID, "-t", "first"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("send failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdMsg, "--mark-read"}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatal("first read failed")
	}
	var out bytes.Buffer
	if code := Run([]string{tcCmdMsg}, &out, &bytes.Buffer{}); code != 0 {
		t.Fatalf("second read exit %d", code)
	}
	if !strings.Contains(out.String(), "✓ no messages") {
		t.Errorf("expected no messages after mark-read; got %q", out.String())
	}
}
