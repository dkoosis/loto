package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"loto/internal/domain"
)

// mkGoneMovedPath returns an absolute path that once existed and no longer
// does — the exact "old checkout path" shape RepairWorktreeStamps' automatic
// detection requires (worktreePathGone). Built by actually creating and then
// removing a directory, rather than a string that never existed on this
// machine, so the test exercises the real os.Stat/ErrNotExist path a moved or
// renamed worktree leaves behind.
func mkGoneMovedPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "moved-away")
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(p); err != nil {
		t.Fatal(err)
	}
	return p
}

// claimWorktreeColumn reads the claims.worktree column directly rather than
// through ListClaims, whose loadClaimsTx never SELECTs it (a known,
// unrelated gap the loto-in0v bead comment flags — Worktree always reads ""
// through that path regardless of what is stored).
func claimWorktreeColumn(t *testing.T, s *Store, prefix, owner string) string {
	t.Helper()
	var wt string
	err := s.db.QueryRowContext(context.Background(),
		`SELECT worktree FROM claims WHERE path_prefix = ? AND owner_uuid = ?`, prefix, owner).Scan(&wt)
	if err != nil {
		t.Fatalf("read claims.worktree: %v", err)
	}
	return wt
}

// TestCollectStaleWorktreeStamps_ReportsCounts is loto-in0v's audit-side AC:
// `loto doctor` without --repair reports stale-path rows, one entry per old
// path with lock+claim counts (Rules). Not owner-scoped — this is read-only
// triage, unlike RepairWorktreeStamps below.
func TestCollectStaleWorktreeStamps_ReportsCounts(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.ClaimPrefix(ctx, domain.ClaimRecord{
		PathPrefix:  "internal/store",
		OwnerUUID:   tcAlice,
		SessionUUID: "sess",
		Intent:      "moved territory",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		Worktree:    oldPath,
	}, liveProbe); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, "h", true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleWorktreeStamps) != 1 {
		t.Fatalf("want 1 stale-worktree-stamp row, got %+v", report.StaleWorktreeStamps)
	}
	got := report.StaleWorktreeStamps[0]
	if got.OldPath != oldPath || got.Locks != 1 || got.Claims != 1 {
		t.Errorf("want old=%s locks=1 claims=1, got %+v", oldPath, got)
	}
}

// TestCollectStaleWorktreeStamps_LiveWorktreeNotReported is the Rules'
// "never rewrite a stamp whose path still exists" invariant, applied to the
// read side too: a worktree path that still stands on disk (a live sibling
// checkout, or simply this same machine's real cwd) must not be reported as
// stale.
func TestCollectStaleWorktreeStamps_LiveWorktreeNotReported(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	livePath := t.TempDir() // exists on disk

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", livePath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	report, err := s.DoctorAudit(ctx, "h", true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleWorktreeStamps) != 0 {
		t.Fatalf("a still-existing worktree path must not be reported stale, got %+v", report.StaleWorktreeStamps)
	}
}

// TestRepairWorktreeStamps_RewritesOwnedLockAndClaim is loto-in0v's core AC:
// a moved worktree's own lock and claim rows are rewritten to the new
// checkout path, and a second doctor run shows no more stale stamps.
func TestRepairWorktreeStamps_RewritesOwnedLockAndClaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)
	newPath := t.TempDir()

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.ClaimPrefix(ctx, domain.ClaimRecord{
		PathPrefix:  "internal/store",
		OwnerUUID:   tcAlice,
		SessionUUID: "sess",
		Intent:      "moved territory",
		CreatedAt:   now,
		ExpiresAt:   now.Add(time.Hour),
		Host:        "h",
		Worktree:    oldPath,
	}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 2 {
		t.Fatalf("want 2 rows repaired (lock+claim), got %+v", repaired)
	}

	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != newPath {
		t.Fatalf("lock worktree not rewritten to %s: %+v", newPath, got)
	}
	if wt := claimWorktreeColumn(t, s, "internal/store", tcAlice); wt != newPath {
		t.Fatalf("claim worktree not rewritten: got %q, want %q", wt, newPath)
	}

	report, err := s.DoctorAudit(ctx, "h", true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleWorktreeStamps) != 0 {
		t.Fatalf("expected no stale stamps after repair, got %+v", report.StaleWorktreeStamps)
	}
}

// TestRepairWorktreeStamps_LiveSiblingNeverRewritten is the Rules' "never
// rewrite a stamp whose path still exists" invariant on the write side:
// alice's repair must not fire on a row whose stamped path still stands on
// disk, even when she names that path with --moved-from. The live-sibling
// case (bob's row untouched by alice's repair) is covered by
// TestRepairWorktreeStamps_TwoLiveWorktreesRepairFromOneLeavesOtherUntouched.
func TestRepairWorktreeStamps_LiveSiblingNeverRewritten(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	liveOldPath := t.TempDir() // alice's own "old" path, but still exists on disk
	newPath := t.TempDir()

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", liveOldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, liveOldPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 0 {
		t.Fatalf("a row whose stamped path still exists must never be rewritten, got %+v", repaired)
	}
	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != liveOldPath {
		t.Fatalf("lock worktree must be untouched, got %+v", got)
	}
}

// TestRepairWorktreeStamps_TwoLiveWorktreesRepairFromOneLeavesOtherUntouched
// is AC2: alice repairs her own moved checkout; bob's own live sibling
// checkout's rows (different owner entirely) are untouched regardless of
// their path's on-disk state.
func TestRepairWorktreeStamps_TwoLiveWorktreesRepairFromOneLeavesOtherUntouched(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	aliceOld := mkGoneMovedPath(t)
	aliceNew := t.TempDir()
	bobWt := t.TempDir()

	a := mkFileLockSessionWorktree(t, "a.go", tcAlice, "alice-sess", "fixer-a", aliceOld, time.Hour)
	b := mkFileLockSessionWorktree(t, "b.go", tcBob, "bob-sess", "fixer-b", bobWt, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, aliceNew, aliceOld)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || repaired[0].Canonical != a.Target.Canonical {
		t.Fatalf("want only alice's a.go repaired, got %+v", repaired)
	}

	gotBob, err := s.LockAt(ctx, b.Target)
	if err != nil {
		t.Fatal(err)
	}
	if gotBob == nil || gotBob.Worktree != bobWt {
		t.Fatalf("bob's row must be untouched by alice's repair, got %+v", gotBob)
	}
}

// TestRepairWorktreeStamps_CollisionKeepsOneRow is AC3: a lock row already
// stamped with the new worktree (the owner re-acquired the same target from
// its new location before running --repair) plus a stale row for the same
// target and owner collapse to the one row already at the new path — never
// two rows for one owner+target (Rules).
func TestRepairWorktreeStamps_CollisionKeepsOneRow(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)
	newPath := t.TempDir()

	fileDir := t.TempDir()
	target := filepath.Join(fileDir, "a.go")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	base := domain.LockRecord{
		Target:    domain.Target{Canonical: target},
		OwnerUUID: tcAlice,
		Intent:    "work",
		CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
		Host:      "h",
		PID:       1,
	}
	stale := base
	stale.SessionUUID = "sess-old"
	stale.Worktree = oldPath
	fresh := base
	fresh.SessionUUID = "sess-new"
	fresh.Worktree = newPath

	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{fresh}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || repaired[0].Kind != repairKindLock || repaired[0].OldPath != oldPath {
		t.Fatalf("want the collided stale row reported once, got %+v", repaired)
	}

	holders, err := s.LocksAt(ctx, base.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(holders) != 1 {
		t.Fatalf("want exactly one surviving row after the collision collapse, got %+v", holders)
	}
	if holders[0].Worktree != newPath || holders[0].SessionUUID != fresh.SessionUUID {
		t.Fatalf("the surviving row must be the one already at newPath, got %+v", holders[0])
	}
}

// TestRepairWorktreeStamps_MovedFromScopesToNamedPath: with two distinct
// stale old paths owned by the same agent, --moved-from repairs only the
// one named, leaving the other for a later, separate run.
func TestRepairWorktreeStamps_MovedFromScopesToNamedPath(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldA := mkGoneMovedPath(t)
	oldB := mkGoneMovedPath(t)
	newPath := t.TempDir()

	a := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", oldA, time.Hour)
	b := mkFileLockSessionWorktree(t, "b.go", tcAlice, "sess", "fixer-a", oldB, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, oldA)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 || repaired[0].OldPath != oldA {
		t.Fatalf("want only oldA's row repaired, got %+v", repaired)
	}

	gotB, err := s.LockAt(ctx, b.Target)
	if err != nil {
		t.Fatal(err)
	}
	if gotB == nil || gotB.Worktree != oldB {
		t.Fatalf("oldB's row must be left for its own --moved-from run, got %+v", gotB)
	}
}

// TestRepairWorktreeStamps_NoMovedFromRewritesNothing is the PR #376 Codex P1
// tightening: owner match plus a vanished path cannot tell a MOVED checkout
// from a DELETED sibling of the same pinned owner (loto-8z87 lets one owner
// hold rows in two worktrees). Without --moved-from, nothing is adopted.
func TestRepairWorktreeStamps_NoMovedFromRewritesNothing(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	deletedSibling := mkGoneMovedPath(t)
	here := t.TempDir()

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-b", deletedSibling, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}

	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, here, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 0 {
		t.Fatalf("no --moved-from: a deleted sibling's row must not be adopted, got %+v", repaired)
	}
	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != deletedSibling {
		t.Fatalf("lock stamp must be untouched, got %+v", got)
	}
}

// TestRepairWorktreeStamps_MovedFromUncleanPathIsCleaned: a trailing slash or
// a `..` segment names the same checkout, so it must match the stamp.
func TestRepairWorktreeStamps_MovedFromUncleanPathIsCleaned(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)
	newPath := t.TempDir()

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	unclean := filepath.Dir(oldPath) + "/x/../" + filepath.Base(oldPath) + "/"
	repaired, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, unclean)
	if err != nil {
		t.Fatal(err)
	}
	if len(repaired) != 1 {
		t.Fatalf("unclean --moved-from %q must match stamp %q, got %+v", unclean, oldPath, repaired)
	}
}

// TestRepairWorktreeStamps_MovedFromRelativeRefused: a relative path can
// never equal an absolute stamp, so it is an error, not a silent no-op.
func TestRepairWorktreeStamps_MovedFromRelativeRefused(t *testing.T) {
	s := mustOpen(t)
	_, err := s.RepairWorktreeStamps(context.Background(), tcAlice, t.TempDir(), "old/checkout")
	if !errors.Is(err, ErrMovedFromNotAbsolute) {
		t.Fatalf("want ErrMovedFromNotAbsolute, got %v", err)
	}
}

// TestRepairWorktreeStamps_CollisionRehomesPendingTag (PR #376 Codex P2): the
// stale row's pending tag must survive the collapse, re-pointed at the lock
// that stays, so the holder still sees it.
func TestRepairWorktreeStamps_CollisionRehomesPendingTag(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)
	newPath := t.TempDir()

	target := filepath.Join(t.TempDir(), "a.go")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	stale := domain.LockRecord{
		Target: domain.Target{Canonical: target}, OwnerUUID: tcAlice, SessionUUID: "sess-old",
		Intent: "work", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1, Worktree: oldPath,
	}
	fresh := stale
	fresh.SessionUUID = "sess-new"
	fresh.CreatedAt = now.Add(time.Second)
	fresh.Worktree = newPath
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{fresh}, liveProbe); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(stale.Target.Canonical), LockOwnerUUID: string(tcAlice),
		LockCreatedAt: stale.CreatedAt.UnixNano(), TaggerUUID: string(tcBob), Text: "heads up",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.RepairWorktreeStamps(ctx, tcAlice, newPath, oldPath); err != nil {
		t.Fatal(err)
	}
	tags, err := s.ListAliveForOwner(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) != 1 || tags[0].Text != "heads up" {
		t.Fatalf("the stale row's pending tag must survive the collapse on the kept lock, got %+v", tags)
	}
}

// TestPreviewWorktreeStampRepair_ReportsWithoutWriting (PR #376 Codex P2 /
// cubic P2): --dry-run must name the rows --repair --moved-from would rewrite,
// and leave them as they are.
func TestPreviewWorktreeStampRepair_ReportsWithoutWriting(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)
	newPath := t.TempDir()

	l := mkFileLockSessionWorktree(t, "a.go", tcAlice, "sess", "fixer-a", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{l}, liveProbe); err != nil {
		t.Fatal(err)
	}
	would, err := s.PreviewWorktreeStampRepair(ctx, tcAlice, newPath, oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(would) != 1 {
		t.Fatalf("preview must report the one row, got %+v", would)
	}
	got, err := s.LockAt(ctx, l.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Worktree != oldPath {
		t.Fatalf("preview must not write, got %+v", got)
	}
}

// TestCollectStaleWorktreeStamps_NamesOwners: stamp repair is owner-scoped,
// so the report carries who owns each stale path (cubic P2 on #376).
func TestCollectStaleWorktreeStamps_NamesOwners(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	oldPath := mkGoneMovedPath(t)

	a := mkFileLockSessionWorktree(t, "a.go", tcBob, "s1", "fixer-a", oldPath, time.Hour)
	b := mkFileLockSessionWorktree(t, "b.go", tcAlice, "s2", "fixer-a", oldPath, time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{a, b}, liveProbe); err != nil {
		t.Fatal(err)
	}
	report, err := s.DoctorAudit(ctx, "h", true, liveProbe, SidecarCheck{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.StaleWorktreeStamps) != 1 {
		t.Fatalf("want one row, got %+v", report.StaleWorktreeStamps)
	}
	got := report.StaleWorktreeStamps[0]
	want := []string{string(tcAlice), string(tcBob)}
	slices.Sort(want)
	if got.Locks != 2 || !slices.Equal(got.Owners, want) {
		t.Fatalf("want locks=2 owners=%v, got %+v", want, got)
	}
}
