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

// TestLiveOwnerUUIDsInRepo_RotatedSelfIDIsNotAPeer is loto-2jgn. Claude Code's
// /clear rotates the session id without ending the process, so `loto whoami`
// leaves a SECOND record naming the same (pid, proc_start). Both verdict live
// and a top-level session's owner uuid IS its session id, so |S| read 2 in a
// checkout holding one session — and the ref guard refused every branch
// create, worktree add and branch delete in it.
func TestLiveOwnerUUIDsInRepo_RotatedSelfIDIsNotAPeer(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", existingSocket(t))
	const repo = "/repos/a"

	record(t, "sess-precleared", "owner-precleared", repo)
	// record leaves its sid in the environment, so this one is the caller.
	record(t, "sess-current", "owner-current", repo)

	live := LiveOwnerUUIDsInRepo(repo)
	if len(live) != 1 {
		t.Fatalf("|S| = %d, want 1 — one process is one peer, whatever its id history: %v", len(live), live)
	}
	if _, ok := live["owner-current"]; !ok {
		t.Errorf("the surviving entry must be the caller's own owner: %v", live)
	}
}

// TestLiveOwnerUUIDsInRepo_PeerProcessStillCounts pins the other direction of
// loto-2jgn's fix: the collapse is keyed on the process, so a session in a
// DIFFERENT process is still a peer and still protects its tree. The test
// binary's parent stands in for that process — a pid that is genuinely live
// and provably not this one.
func TestLiveOwnerUUIDsInRepo_PeerProcessStillCounts(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", existingSocket(t))
	const repo = "/repos/a"

	t.Setenv("LOTO_PID", strconv.Itoa(os.Getppid()))
	record(t, "sess-peer", "owner-peer", repo)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	record(t, "sess-precleared", "owner-precleared", repo)
	record(t, "sess-current", "owner-current", repo)

	live := LiveOwnerUUIDsInRepo(repo)
	if len(live) != 2 {
		t.Fatalf("|S| = %d, want 2 — me and one peer process: %v", len(live), live)
	}
	if _, ok := live["owner-peer"]; !ok {
		t.Errorf("a session in another process must stay in S: %v", live)
	}
}

// TestLiveOwnerUUIDsInRepo_NoSelfRecordCollapsesNothing: the collapse needs
// the caller's own record as the entry it keeps. Without one — a session that
// never ran `loto whoami` here — nothing is dropped, so the count can only be
// too high, never too low. Over-counting refuses a ref update; under-counting
// would hand a peer's working tree to a branch switch.
func TestLiveOwnerUUIDsInRepo_NoSelfRecordCollapsesNothing(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", existingSocket(t))
	const repo = "/repos/a"

	record(t, "sess-one", "owner-one", repo)
	record(t, "sess-two", "owner-two", repo)
	t.Setenv("CLAUDE_CODE_SESSION_ID", "sess-unrecorded")

	if got := len(LiveOwnerUUIDsInRepo(repo)); got != 2 {
		t.Errorf("|S| = %d, want 2 — with no record of its own the caller collapses nothing", got)
	}
}

func record(t *testing.T, sid, owner, repo string) {
	t.Helper()
	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	if _, err := RecordSession(&Agent{UUID: owner, Host: "h"}, repo); err != nil {
		t.Fatalf("RecordSession(%s): %v", sid, err)
	}
}
