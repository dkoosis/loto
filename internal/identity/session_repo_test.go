package identity

import (
	"os"
	"strconv"
	"testing"
)

// TestLiveOwnerUUIDsInRepo_ScopesToOneCheckout is loto-ea8y.4's review fix.
// The unscoped set is machine-wide, so a session live in an unrelated repo
// counted toward I1's |S| and made a checkout with no peer in it refuse a
// branch switch, a stash and a branch delete.
func TestLiveOwnerUUIDsInRepo_ScopesToOneCheckout(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	socket := existingSocket(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)

	const repoA, repoB = "/repos/a", "/repos/b"
	record(t, "sess-a1", "owner-a1", repoA)
	record(t, "sess-a2", "owner-a2", repoA)
	record(t, "sess-b1", "owner-b1", repoB)

	if got := len(LiveOwnerUUIDsInRepo(repoA)); got != 2 {
		t.Errorf("|S| in repo A = %d, want 2", got)
	}
	// The point of the fix: repo B has ONE live session, not three.
	live := LiveOwnerUUIDsInRepo(repoB)
	if len(live) != 1 {
		t.Fatalf("|S| in repo B = %d, want 1 — sessions live elsewhere are not peers here", len(live))
	}
	if _, ok := live["owner-b1"]; !ok {
		t.Errorf("repo B's set must be its own session: %v", live)
	}
	// The machine-wide question still has its answer, for doctor's stash
	// triage, which asks "is this owner doing anything anywhere".
	if got := len(LiveOwnerUUIDs()); got != 3 {
		t.Errorf("unscoped LiveOwnerUUIDs = %d, want all 3", got)
	}
}

// TestLiveOwnerUUIDsInRepo_UnknownRepoIsNotHere: a record written before the
// repo field existed cannot be shown to be in this checkout. I1's failure
// direction is to pass, so it is counted out rather than counted in.
func TestLiveOwnerUUIDsInRepo_UnknownRepoIsNotHere(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", existingSocket(t))

	record(t, "sess-legacy", "owner-legacy", "")
	record(t, "sess-here", "owner-here", "/repos/a")

	if got := len(LiveOwnerUUIDsInRepo("/repos/a")); got != 1 {
		t.Errorf("|S| = %d, want 1 — a repo-less record names no checkout", got)
	}
	// And an empty question has an empty answer: a caller with no checkout to
	// name has no checkout to scope to.
	if got := len(LiveOwnerUUIDsInRepo("")); got != 0 {
		t.Errorf("LiveOwnerUUIDsInRepo(\"\") = %d, want 0", got)
	}
}

func record(t *testing.T, sid, owner, repo string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	if _, err := RecordSession(&Agent{UUID: owner, Host: "h"}, repo); err != nil {
		t.Fatalf("RecordSession(%s): %v", sid, err)
	}
}
