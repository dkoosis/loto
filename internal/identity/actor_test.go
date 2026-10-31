package identity

import (
	"context"
	"os"
	"strconv"
	"testing"
)

// ensureAs resolves the owner for one session id on one session pid.
func ensureAs(t *testing.T, sid string, pid int) string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	t.Setenv("LOTO_PID", strconv.Itoa(pid))
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure(%s, pid %d): %v", sid, pid, err)
	}
	return a.UUID
}

// The bead's first acceptance criterion at the identity layer (loto-e1ku):
// the actor is the Claude Code process, so two session ids on one pid (a
// /clear, or a subagent) are one owner, and another process is another.
func TestEnsureActorIsTheProcess(t *testing.T) {
	clearIdentityEnv(t)
	stubProcStart(t, 4242, true)
	before := ensureAs(t, "sess-before-clear", os.Getpid())
	after := ensureAs(t, "sess-after-clear", os.Getpid())
	if before != after {
		t.Fatalf("two session ids on one pid resolved to %s and %s, want one actor", before, after)
	}
	if before == "sess-before-clear" {
		t.Fatal("owner is the session id, want the actor derived from the process")
	}
	if peer := ensureAs(t, "sess-peer", os.Getpid()+1); peer == before {
		t.Fatalf("another pid resolved to the same owner %s", peer)
	}
}

// A hook spawned without the session's pid in its env names the same actor
// from the session record whoami wrote; with no process known at all, the
// owner stays the session id.
func TestEnsureActorFromSessionRecord(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LOTO_BASE", "")
	stubProcStart(t, 4242, true)
	want := ensureAs(t, "sess-rec", os.Getpid())
	if _, err := RecordSession(&Agent{UUID: want}, ""); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	unsetEnv(t, "LOTO_PID")
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if a.UUID != want {
		t.Fatalf("owner from session record = %s, want %s", a.UUID, want)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-unrecorded")
	if a, err = Ensure(context.Background()); err != nil || a.UUID != "sess-unrecorded" {
		t.Fatalf("no process known: owner = %v (err %v), want the session id", a, err)
	}
}

// A claude started from a session's Bash inherits that session's LOTO_PID;
// its own CLAUDE_PID names its actor, not its parent's.
func TestEnsureActorPrefersClaudePID(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_BASE", t.TempDir())
	stubProcStart(t, 4242, true)
	parent := ensureAs(t, "sess-child", os.Getpid())
	t.Setenv("CLAUDE_PID", strconv.Itoa(os.Getpid()+1))
	child, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if child.UUID == parent {
		t.Fatalf("child with its own CLAUDE_PID resolved to the inherited LOTO_PID's actor %s", parent)
	}
	rec, err := RecordSession(child, "")
	if err != nil {
		t.Fatal(err)
	}
	if rec.PID != os.Getpid()+1 {
		t.Fatalf("child record pid = %d, want its CLAUDE_PID %d", rec.PID, os.Getpid()+1)
	}
	unsetEnv(t, "CLAUDE_PID")
	unsetEnv(t, "LOTO_PID")
	fromRecord, err := Ensure(context.Background())
	if err != nil || fromRecord.UUID != child.UUID {
		t.Fatalf("child resolved from its record = %v (err %v), want %s", fromRecord, err, child.UUID)
	}
}
