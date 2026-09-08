package store

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestReleaseBySession_ReleasesLocksAndClaimsAtomically proves the ei5 fix:
// ONE ReleaseBySession call clears both the session's locks and its claims in a
// single transaction (Codex #219 P1 — no second claim tx that could leave claims
// squatting if the first committed). A sibling session's lock AND claim survive.
func TestReleaseBySession_ReleasesLocksAndClaimsAtomically(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLockSession(t, "a.go", tcAlice, "session-1", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaimSession(tcPkgStore, tcAlice, "session-1", time.Hour), nil); err != nil {
		t.Fatal(err)
	}
	// Sibling session-2 territory that must survive.
	lb := mkFileLockSession(t, "b.go", tcAlice, "session-2", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{lb}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaimSession("internal/render", tcAlice, "session-2", time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", "")
	results, claims := rel.Results, rel.ClaimPrefixes
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(results) != 1 || results[0].Target.Canonical != la.Target.Canonical {
		t.Fatalf("locks released = %v, want just a.go", results)
	}
	if len(claims) != 1 || claims[0] != tcPkgStore {
		t.Fatalf("claims released = %v, want [internal/store]", claims)
	}
	// session-2's lock and claim both survive.
	if got, _ := s.LockAt(ctx, lb.Target); got == nil {
		t.Error("session-2 lock should survive session-1 release")
	}
	remaining, _ := s.ListClaims(ctx)
	if len(remaining) != 1 || remaining[0].PathPrefix != "internal/render" {
		t.Errorf("surviving claims = %v, want just session-2's internal/render", remaining)
	}
}

// TestReleaseBySession_ScopedToSession verifies that ReleaseBySession only
// releases locks matching both agent UUID and session UUID — sibling sessions
// of the same agent are left intact.
func TestReleaseBySession_ScopedToSession(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLockSession(t, "a.go", tcAlice, "session-1", time.Hour)
	lb := mkFileLockSession(t, "b.go", tcAlice, "session-2", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb}, liveProbe); err != nil {
		t.Fatal(err)
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", "")
	results := rel.Results
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].State != StateUnlocked {
		t.Errorf("want StateUnlocked, got %v", results[0].State)
	}
	if results[0].Target.Canonical != la.Target.Canonical {
		t.Errorf("wrong target: got %s, want %s", results[0].Target.Canonical, la.Target.Canonical)
	}

	// session-2's lock must survive.
	got, err := s.LockAt(ctx, lb.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("session-2's lock should survive session-1 release")
	}
}

// TestReleaseBySession_AgentScoped verifies that when sessionUUID is empty,
// ReleaseBySession releases all locks owned by the agent regardless of session.
func TestReleaseBySession_AgentScoped(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	la := mkFileLockSession(t, "a.go", tcAlice, "session-1", time.Hour)
	lb := mkFileLockSession(t, "b.go", tcAlice, "session-2", time.Hour)
	lc := mkFileLockSession(t, "c.go", tcBob, "session-3", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{la, lb, lc}, liveProbe); err != nil {
		t.Fatal(err)
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "", "")
	results := rel.Results
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results (both alice sessions), got %d", len(results))
	}
	for _, r := range results {
		if r.State != StateUnlocked {
			t.Errorf("want StateUnlocked for %s, got %v", r.Target.Canonical, r.State)
		}
	}

	// Bob's lock must survive.
	got, err := s.LockAt(ctx, lc.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("bob's lock should survive alice's release")
	}
}

// TestReleaseBySession_EmptyResult verifies that releasing with no matching
// locks returns an empty result set without error.
func TestReleaseBySession_EmptyResult(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	rel, err := s.ReleaseBySession(ctx, tcAlice, "no-such-session", "")
	results := rel.Results
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want 0 results, got %d", len(results))
	}
}

// TestReleaseBySession_LeavesModeUntouched is loto-zssw's session-release leg:
// acquire strips nothing, so release restores nothing, and the file's mode is
// the same on both sides of the lock's whole life.
func TestReleaseBySession_LeavesModeUntouched(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	l := mkFileLockSession(t, "x.go", tcAlice, "session-1", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("acquire must not touch mode bits, got %o", st.Mode().Perm())
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", "")
	results := rel.Results
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != StateUnlocked {
		t.Fatalf("want StateUnlocked, got %+v", results)
	}
	st, _ = os.Stat(l.Target.Canonical)
	if st.Mode().Perm() != 0o644 {
		t.Fatalf("release must not touch mode bits, got %o", st.Mode().Perm())
	}
}

// mkFileLockSession is like mkFileLock but takes an explicit session UUID.
func mkFileLockSession(t *testing.T, name, agent, session string, expIn time.Duration) domain.LockRecord {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	return domain.LockRecord{
		Target:      domain.Target{Canonical: p},
		OwnerUUID:   domain.AgentUUID(agent),
		SessionUUID: domain.SessionUUID(session),
		Intent:      tcTest,
		CreatedAt:   now,
		ExpiresAt:   now.Add(expIn),
		Host:        "h",
		PID:         1,
	}
}

// The two lane intents the scoped-release tests share. Named because goconst
// counts repeats across the package, and because "which lane took this lock"
// is the whole discriminator these tests exercise.
const (
	tcLaneAIntent = "laneA: work"
	tcLaneBIntent = "laneB: work"
)

// TestReleaseBySession_OnlyIntent_SelectsAndDeletesInOneTx pins loto-lzap's
// second P1: the intent filter is part of the release transaction, not a
// caller-side list-then-delete. Two locks under one owner and session carry
// different intents; releasing by one intent leaves the other row standing and
// reports only the intent it actually swept.
//
// The CLI used to do this filtering itself — ListLocks, keep the matching
// intents, ReleaseLocks those targets — and lost the race it was written to
// win: insertOrRefreshLock upserts `intent` on the (target_canonical,
// owner_uuid) PK, so a same-owner peer re-acquiring between the list and the
// delete rewrote the intent, and ReleaseLocks classified on target and owner
// alone. Filtering in SQL makes that sequence unrepresentable.
func TestReleaseBySession_OnlyIntent_SelectsAndDeletesInOneTx(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	mine := mkFileLockSession(t, "a.go", tcAlice, "session-1", time.Hour)
	mine.Intent = tcLaneAIntent
	peer := mkFileLockSession(t, "b.go", tcAlice, "session-1", time.Hour)
	peer.Intent = tcLaneBIntent
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{mine, peer}, liveProbe); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", tcLaneAIntent)
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(rel.Results) != 1 || rel.Results[0].Target.Canonical != mine.Target.Canonical {
		t.Fatalf("only the matching-intent lock may be released, got %+v", rel.Results)
	}
	if len(rel.Intents) != 1 || rel.Intents[0] != tcLaneAIntent {
		t.Errorf("Intents must name what was swept, got %v", rel.Intents)
	}

	left, err := s.ListLocks(ctx)
	if err != nil {
		t.Fatalf("ListLocks: %v", err)
	}
	if len(left) != 1 || left[0].Target.Canonical != peer.Target.Canonical {
		t.Errorf("the peer lane's different-intent lock must survive, got %+v", left)
	}
}

// TestReleaseBySession_OnlyIntent_KeepsClaims: a filtered release names locks a
// lane took, not the territory it reserved. Claims carry no intent to match, so
// sweeping them here would drop the one thing the filter exists to leave
// standing.
func TestReleaseBySession_OnlyIntent_KeepsClaims(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	l := mkFileLockSession(t, "a.go", tcAlice, "session-1", time.Hour)
	l.Intent = tcLaneAIntent
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimPrefix(ctx, mkClaimSession(tcPkgStore, tcAlice, "session-1", time.Hour), nil); err != nil {
		t.Fatal(err)
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", tcLaneAIntent)
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(rel.ClaimPrefixes) != 0 {
		t.Errorf("a filtered release must not touch claims, got %v", rel.ClaimPrefixes)
	}
	claims, err := s.ListClaims(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Errorf("the claim must survive a filtered release, got %+v", claims)
	}
}

// TestReleaseBySession_ReportsEveryIntentItSwept pins loto-lzap's third P1: the
// peer-intent warning reads the intents of the rows the transaction actually
// deleted. Reading them from a listing BEFORE the release could observe one
// intent while the sweep took two — announcing nothing about the very peer lock
// it removed.
func TestReleaseBySession_ReportsEveryIntentItSwept(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	for name, intent := range map[string]string{"a.go": tcLaneAIntent, "b.go": tcLaneBIntent, "c.go": tcLaneAIntent} {
		rec := mkFileLockSession(t, name, tcAlice, "session-1", time.Hour)
		rec.Intent = intent
		if _, err := s.AcquireLocks(ctx, []domain.LockRecord{rec}, liveProbe); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}

	rel, err := s.ReleaseBySession(ctx, tcAlice, "session-1", "")
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(rel.Results) != 3 {
		t.Fatalf("the unfiltered sweep must take all three, got %d", len(rel.Results))
	}
	want := []string{tcLaneAIntent, tcLaneBIntent}
	if len(rel.Intents) != len(want) {
		t.Fatalf("want the two distinct intents, got %v", rel.Intents)
	}
	for i := range want {
		if rel.Intents[i] != want[i] {
			t.Errorf("Intents must be deduplicated and sorted: got %v, want %v", rel.Intents, want)
		}
	}
}
