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

	results, claims, err := s.ReleaseBySession(ctx, tcAlice, "session-1")
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

	results, _, err := s.ReleaseBySession(ctx, tcAlice, "session-1")
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

	results, _, err := s.ReleaseBySession(ctx, tcAlice, "")
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

	results, _, err := s.ReleaseBySession(ctx, tcAlice, "no-such-session")
	if err != nil {
		t.Fatalf("ReleaseBySession: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("want 0 results, got %d", len(results))
	}
}

// TestReleaseBySession_RestoresChmod verifies that released locks get their
// chmod restored (owner-write re-added).
func TestReleaseBySession_RestoresChmod(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	l := mkFileLockSession(t, "x.go", tcAlice, "session-1", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	// Verify stripped.
	st, _ := os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("precondition: acquire should strip write, got %o", st.Mode().Perm())
	}

	results, _, err := s.ReleaseBySession(ctx, tcAlice, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].State != StateUnlocked {
		t.Fatalf("want StateUnlocked, got %+v", results)
	}
	st, _ = os.Stat(l.Target.Canonical)
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("release must restore owner-write, got %o", st.Mode().Perm())
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
