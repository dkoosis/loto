package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"loto/internal/identity"
)

// mintAgent creates a fresh persisted identity (a distinct session) in the
// current HOME and returns it. Mirrors twoAgents' minting for the 4-actor
// shared/exclusive acceptance scenario (loto-k5el.2 T9).
func mintAgent(t *testing.T, name string) *identity.Agent {
	t.Helper()
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("%s-%d-%d", name, time.Now().UnixNano(), pinCounter.Add(1)))
	a, err := identity.Ensure(context.Background())
	if err != nil {
		t.Fatalf("mint %s: %v", name, err)
	}
	return a
}

// TestAcceptance_SharedExclusiveDowngrade is the end-to-end SC walk-through:
// shared+shared coexist, exclusive conflicts with shared holders, exclusive
// acquires once they release, then downgrade reopens the target to shared peers.
func TestAcceptance_SharedExclusiveDowngrade(t *testing.T) {
	withTempProject(t)
	alice := mintAgent(t, "alice")
	bob := mintAgent(t, "bob")
	carol := mintAgent(t, "carol")
	dave := mintAgent(t, "dave")

	runAs := func(a *identity.Agent, args ...string) (int, string) {
		t.Helper()
		t.Setenv("LOTO_AGENT_ID", a.UUID)
		var out, errBuf bytes.Buffer
		code := Run(args, &out, &errBuf)
		return code, out.String() + errBuf.String()
	}

	// 1. two shared locks coexist.
	if code, out := runAs(alice, tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared); code != 0 {
		t.Fatalf("alice shared: code=%d %s", code, out)
	}
	if code, out := runAs(bob, tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared); code != 0 {
		t.Fatalf("shared+shared must coexist: code=%d %s", code, out)
	}
	// 2. exclusive conflicts with the shared holders.
	if code, _ := runAs(carol, tcCmdLock, tcTargetA, "-t", tcIntentWrite); code != 1 {
		t.Fatalf("exclusive must conflict with existing shared holders, got code=%d", code)
	}
	// 3. release shared holders, then exclusive succeeds.
	if code, out := runAs(alice, tcCmdUnlock, tcTargetA, "-t", tcIntentDone); code != 0 {
		t.Fatalf("alice unlock: code=%d %s", code, out)
	}
	if code, out := runAs(bob, tcCmdUnlock, tcTargetA, "-t", tcIntentDone); code != 0 {
		t.Fatalf("bob unlock: code=%d %s", code, out)
	}
	if code, out := runAs(carol, tcCmdLock, tcTargetA, "-t", tcIntentWrite); code != 0 {
		t.Fatalf("exclusive should acquire once shared holders gone: code=%d %s", code, out)
	}
	// 4. carol downgrades; dave can then take shared.
	if code, out := runAs(carol, tcCmdDowngrade, tcTargetA); code != 0 {
		t.Fatalf("downgrade should succeed: code=%d %s", code, out)
	}
	if code, out := runAs(dave, tcCmdLock, tcTargetA, "-t", tcIntentRead, tcFlagShared); code != 0 {
		t.Fatalf("shared should succeed after downgrade: code=%d %s", code, out)
	}
}
