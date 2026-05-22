package cli

import (
	"bytes"
	"strings"
	"testing"
)

// must0 runs Run with the given argv and fails the test if exit != 0.
func must0(t *testing.T, argv []string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run(argv, &out, &errBuf)
	if code != 0 {
		t.Fatalf("Run %v exit=%d out=%q err=%q", argv, code, out.String(), errBuf.String())
	}
}

func TestCmdTag_AddsExternalTag(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{"tag", tcTargetA, "why", "the", "refactor"}, &out, &errBuf); code != 0 {
		t.Fatalf("tag exit=%d err=%q", code, errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "✓ tag id=t-") {
		t.Fatalf("expected ✓ tag id=t-…: %q", out.String())
	}
	if !strings.Contains(out.String(), "target=a.go") {
		t.Fatalf("expected target=a.go: %q", out.String())
	}
}

func TestCmdTag_RejectsUnlockedTarget(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{"tag", tcTargetA, "ping"}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("expected exit 3, got %d; err=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "not locked") {
		t.Fatalf("expected 'not locked' message, got %q", errBuf.String())
	}
}

func TestCmdTag_SelfTagAccepted(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})
	// Holder tags their own lock (edge #2): accepted, no echo to self.
	var out, errBuf bytes.Buffer
	if code := Run([]string{"tag", tcTargetA, "self note"}, &out, &errBuf); code != 0 {
		t.Fatalf("self-tag should be accepted; exit=%d err=%q", code, errBuf.String())
	}
}

func TestCmdTag_CapAt5(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	for i := range 5 {
		var out, errBuf bytes.Buffer
		code := Run([]string{"tag", tcTargetA, "note"}, &out, &errBuf)
		if code != 0 {
			t.Fatalf("tag %d should succeed; exit=%d err=%q", i, code, errBuf.String())
		}
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{"tag", tcTargetA, "overflow"}, &out, &errBuf)
	if code != 3 {
		t.Fatalf("6th tag must fail; exit=%d out=%q err=%q", code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "cap reached") {
		t.Fatalf("expected 'cap reached' message; err=%q", errBuf.String())
	}
}

func TestStatus_SingleTarget_SurfacesTags(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	must0(t, []string{"tag", tcTargetA, "ETA?"})

	// Non-holder bob runs status — sees the tag inline.
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcTargetA}, &out, &errBuf); code != 0 {
		t.Fatalf("status exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "ETA?") {
		t.Fatalf("expected tag text in status output: %q", out.String())
	}
}

func TestStatus_HolderSeesTrailingFooter(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	must0(t, []string{tcCmdLock, tcTargetA, "-t", tcIntentTest})
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	must0(t, []string{"tag", tcTargetA, "external note"})

	// Alice (holder) runs `status` with no args — trailing footer should fire.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdStatus}, &out, &errBuf); code != 0 {
		t.Fatalf("status exit=%d err=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "external note") {
		t.Fatalf("expected trailing tag footer for holder: %q", out.String())
	}
	if !strings.Contains(out.String(), "ℹ tags count=") {
		t.Fatalf("expected footer count header: %q", out.String())
	}
}

func TestCmdTag_UsagePrintsOnShortArgs(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{"tag", tcTargetA}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("expected exit 2 for missing text; got %d", code)
	}
	if !strings.Contains(errBuf.String(), "usage:") {
		t.Fatalf("expected usage line; err=%q", errBuf.String())
	}
}
