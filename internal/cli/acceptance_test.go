package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

func TestAcceptance_GoldenHappyPath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	steps := []struct {
		args []string
		want string
	}{
		{[]string{"whoami"}, "handle:"},
		{[]string{"lock", "a.go", "--intent", "smoke"}, "✓ locked target=a.go"},
		{[]string{"status", "--mine"}, "a.go"},
		{[]string{"tag", "a.go", "note"}, "✓ tagged"},
		{[]string{"inbox"}, ""},
		{[]string{"unlock", "a.go"}, "✓ unlocked target=a.go"},
	}
	for _, s := range steps {
		var out bytes.Buffer
		code := Run(s.args, &out, io.Discard)
		if code != 0 {
			t.Fatalf("%v exit %d: %s", s.args, code, out.String())
		}
		if s.want != "" && !strings.Contains(out.String(), s.want) {
			t.Errorf("%v missing %q in: %s", s.args, s.want, out.String())
		}
	}
}

// TestAcceptance_BasicMultiAgentFlow exercises the full surface across two
// agents in sequence: alice locks, bob's lock conflicts, alice unlocks, bob
// acquires successfully, alice's stale tag flow.
func TestAcceptance_BasicMultiAgentFlow(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{"lock", "internal/store/", "--intent", "refactor"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice lock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{"lock", "internal/store/store.go"}, &out, io.Discard); code != 1 {
		t.Fatalf("expected conflict, got %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "✗ blocked") {
		t.Errorf("expected ✗ blocked: %q", out.String())
	}
	out.Reset()
	if code := Run([]string{"check-paths", "internal/store/store.go"}, &out, io.Discard); code != 1 {
		t.Fatalf("check-paths expected exit 1, got %d", code)
	}
	if !strings.Contains(out.String(), "blocker=") {
		t.Errorf("check-paths missing blocker: %q", out.String())
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{"unlock", "internal/store/"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("alice unlock failed")
	}

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{"lock", "internal/store/store.go"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("bob lock should succeed after alice unlock")
	}
}
