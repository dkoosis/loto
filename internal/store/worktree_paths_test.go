package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestSyncWorktreePath_FirstSighting_RecordsWithoutMigrating pins the
// no-op-on-first-see leg: a (worktreeID, host) never seen before just
// records currentPath — there is nothing to migrate away from, and no
// existing row anywhere is touched.
func TestSyncWorktreePath_FirstSighting_RecordsWithoutMigrating(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, tcAlice, "main", "/wt/original", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	if err := s.SyncWorktreePath(ctx, "wt-1", "h", "/wt/original"); err != nil {
		t.Fatalf("SyncWorktreePath (first sighting): %v", err)
	}

	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != "/wt/original" {
		t.Fatalf("first sighting must not rewrite unrelated rows, got %+v", got)
	}
}

// TestSyncWorktreePath_SteadyState_IsANoOp pins the case that matters on
// every ordinary invocation: a (worktreeID, host) already pointed at
// currentPath changes nothing.
func TestSyncWorktreePath_SteadyState_IsANoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, tcAlice, "main", "/wt/same", time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if err := s.SyncWorktreePath(ctx, "wt-1", "h", "/wt/same"); err != nil {
		t.Fatalf("SyncWorktreePath (seed): %v", err)
	}

	if err := s.SyncWorktreePath(ctx, "wt-1", "h", "/wt/same"); err != nil {
		t.Fatalf("SyncWorktreePath (steady state): %v", err)
	}

	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != "/wt/same" {
		t.Fatalf("steady state must leave rows alone, got %+v", got)
	}
}

// TestSyncWorktreePath_Move_RewritesLocksAndClaimsButNotAStranger is the
// loto-in0v regression at the store layer: once a (worktreeID, host) pair is
// known at oldPath, a later sync with a DIFFERENT currentPath means the
// checkout moved — every lock and claim still stamped with oldPath is
// rewritten to currentPath, and a row belonging to a genuinely different
// worktree (never stamped with oldPath) is left exactly alone.
func TestSyncWorktreePath_Move_RewritesLocksAndClaimsButNotAStranger(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	const oldPath = "/wt/agent-b"
	const newPath = "/wt/agent-b-moved"
	const strangerPath = "/wt/agent-c"

	mine := mkFileLockSessionWorktree(t, "a.go", tcAlice, tcAlice, "main", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{mine}, liveProbe); err != nil {
		t.Fatal(err)
	}
	stranger := mkFileLockSessionWorktree(t, "b.go", tcBob, tcBob, "main", strangerPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stranger}, liveProbe); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claim := domain.ClaimRecord{
		PathPrefix: "internal/store",
		OwnerUUID:  domain.AgentUUID(tcAlice),
		Intent:     tcTest,
		CreatedAt:  now,
		ExpiresAt:  now.Add(time.Hour),
		Host:       "h",
		Worktree:   oldPath,
	}
	if err := s.ClaimPrefix(ctx, claim, liveProbe); err != nil {
		t.Fatal(err)
	}

	// First sighting at oldPath — nothing to migrate yet.
	if err := s.SyncWorktreePath(ctx, "wt-b", "h", oldPath); err != nil {
		t.Fatalf("SyncWorktreePath (seed): %v", err)
	}
	// The checkout moved: same worktree id, new path.
	if err := s.SyncWorktreePath(ctx, "wt-b", "h", newPath); err != nil {
		t.Fatalf("SyncWorktreePath (move): %v", err)
	}

	gotLock, err := s.LockAt(ctx, mine.Target)
	if err != nil {
		t.Fatal(err)
	}
	if gotLock == nil || gotLock.Worktree != newPath {
		t.Fatalf("moved worktree's lock must be rewritten to newPath, got %+v", gotLock)
	}

	// Read the claim's worktree column directly rather than through
	// ListClaims/scanClaimsRows: that read path does not select `worktree` at
	// all (claimCols omits it, unrelated pre-existing gap — flagged on the
	// bead, out of this bead's scope), so it would always read back "" here
	// regardless of what SyncWorktreePath actually wrote.
	var claimWorktree string
	if err := s.db.QueryRowContext(ctx,
		`SELECT worktree FROM claims WHERE path_prefix = ? AND owner_uuid = ?`,
		claim.PathPrefix, string(claim.OwnerUUID)).Scan(&claimWorktree); err != nil {
		t.Fatal(err)
	}
	if claimWorktree != newPath {
		t.Fatalf("moved worktree's claim must be rewritten to newPath, got %q", claimWorktree)
	}

	gotStranger, err := s.LockAt(ctx, stranger.Target)
	if err != nil {
		t.Fatal(err)
	}
	if gotStranger == nil || gotStranger.Worktree != strangerPath {
		t.Fatalf("a different worktree's row must not be touched, got %+v", gotStranger)
	}
}
