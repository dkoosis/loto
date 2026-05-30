package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// TestUnlockAll_ScopedToSession exercises the loto-81n fix: SessionEnd
// release (unlock --all) must not drop locks held by sibling sessions of
// the same agent. Two LOTO_SESSION_IDs share one LOTO_AGENT_ID; --all in
// session-1 must leave session-2's lock intact.
func TestUnlockAll_ScopedToSession(t *testing.T) {
	withTempProject(t)
	pinAgent(t) // sets LOTO_AGENT_ID to a single agent across all Run() calls below

	// Session 1 locks a.go.
	t.Setenv("LOTO_SESSION_ID", "session-one")
	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "s1 work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session-1 lock a.go: exit %d", code)
	}

	// Session 2 locks internal/store/store.go (same agent, different session).
	t.Setenv("LOTO_SESSION_ID", "session-two")
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, "s2 work"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("session-2 lock store.go: exit %d", code)
	}

	// Back to session-1: --all should release only session-1's holdings.
	t.Setenv("LOTO_SESSION_ID", "session-one")
	var out bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "session-1 end"}, &out, io.Discard); code != 0 {
		t.Fatalf("session-1 unlock --all: exit %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "count=1") {
		t.Errorf("session-1 should release exactly 1 lock; got: %s", out.String())
	}

	// Session-2's lock must survive.
	t.Setenv("LOTO_SESSION_ID", "session-two")
	out.Reset()
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, io.Discard); code != 0 {
		t.Fatalf("status --mine: exit %d", code)
	}
	if !strings.Contains(out.String(), "store.go") {
		t.Errorf("session-2's store.go lock should survive session-1's --all; got: %s", out.String())
	}
	if strings.Contains(out.String(), "a.go") {
		t.Errorf("a.go should have been released by session-1's --all; got: %s", out.String())
	}
}

// TestUnlockAll_FallbackWhenNoSessionPin covers the no-LOTO_SESSION_ID path:
// without pinning, --all stays agent-scoped — the safe fallback for direct
// CLI use where each invocation would otherwise mint a different session id
// and --all would match nothing.
func TestUnlockAll_FallbackWhenNoSessionPin(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	t.Setenv("LOTO_SESSION_ID", "") // explicitly unset; each Run() mints a fresh id

	if code := Run([]string{tcCmdLock, tcTargetA, tcFlagIntent, "lock1"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock a.go: exit %d", code)
	}
	if code := Run([]string{tcCmdLock, tcStoreStoreGo, tcFlagIntent, "lock2"}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock store.go: exit %d", code)
	}

	var out bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcFlagAll, "-t", "cleanup"}, &out, io.Discard); code != 0 {
		t.Fatalf("unlock --all: exit %d, out=%s", code, out.String())
	}
	if !strings.Contains(out.String(), "count=2") {
		t.Errorf("no-pin --all should release both agent-owned locks; got: %s", out.String())
	}
}
