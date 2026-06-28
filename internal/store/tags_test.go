package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestSchemaVersionPaired verifies that migrate() sets user_version to the
// expected constant. PRAGMA user_version is set in Go code (not schema.sql)
// because it is not transactional in SQLite.
func TestSchemaVersionPaired(t *testing.T) {
	s := mustOpen(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatal(err)
	}
	if v != schemaUserVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaUserVersion)
	}
}

// acquireForTest acquires a lock for the test and returns its CreatedAt (ns).
func acquireForTest(t *testing.T, s *Store, name, agent string) (domain.LockRecord, int64) {
	t.Helper()
	rec := mkFileLock(t, name, agent, time.Hour)
	live := func(string, int, int64) bool { return true }
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live); err != nil {
		t.Fatalf("AcquireLocks: %v", err)
	}
	got, err := s.LockAt(context.Background(), rec.Target)
	if err != nil || got == nil {
		t.Fatalf("LockAt: %v / %+v", err, got)
	}
	return *got, got.CreatedAt.UnixNano()
}

func TestInsertTag_VisibleToHolder(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            "why?",
	})
	if err != nil {
		t.Fatalf("InsertTag: %v", err)
	}
	got, err := s.ListAliveForOwner(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Text != "why?" {
		t.Fatalf("ListAliveForOwner: %+v", got)
	}
}

func TestInsertTag_CapEnforcedTransactionally(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	for i := range tagCap {
		if _, err := s.InsertTag(ctx, NewTag{
			TargetCanonical: domain.Canonical(lock.Target.Canonical),
			LockOwnerUUID:   tcAlice,
			LockCreatedAt:   lockNs,
			TaggerUUID:      tcBob,
			Text:            fmt.Sprintf("note %d", i),
		}); err != nil {
			t.Fatalf("InsertTag %d: %v", i, err)
		}
	}
	_, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            "overflow",
	})
	if !errEqualsTo(err, ErrTagCapReached) {
		t.Fatalf("want ErrTagCapReached, got %v", err)
	}
}

func TestInsertTag_TextTooLong_Rejects(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	oversized := strings.Repeat("x", tagTextMaxBytes+1)
	_, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            oversized,
	})
	if !errEqualsTo(err, ErrTagTextTooLong) {
		t.Fatalf("want ErrTagTextTooLong, got %v", err)
	}
	if n := rawTagRowCount(t, s); n != 0 {
		t.Fatalf("oversized tag must not be inserted, got %d rows", n)
	}
}

// TestInsertTag_TextAtCap_Accepted pins the boundary: exactly tagTextMaxBytes
// bytes is the largest legal text.
func TestInsertTag_TextAtCap_Accepted(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	atCap := strings.Repeat("x", tagTextMaxBytes)
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            atCap,
	}); err != nil {
		t.Fatalf("at-cap text must be accepted, got %v", err)
	}
}

func TestInsertTag_NoHostLock_Rejects(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	_, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: "/nonexistent/path",
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   12345,
		TaggerUUID:      tcBob,
		Text:            "hello",
	})
	if !errEqualsTo(err, ErrNoHostLock) {
		t.Fatalf("want ErrNoHostLock, got %v", err)
	}
}

func TestInsertTag_SelfTag_NoHolderEcho_ButTargetVisible(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	// Alice tags her own lock (edge #2).
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical),
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcAlice,
		Text:            "self note",
	}); err != nil {
		t.Fatalf("InsertTag self: %v", err)
	}
	// Holder echo path suppresses it.
	holderTags, err := s.ListAliveForOwner(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(holderTags) != 0 {
		t.Fatalf("self-tag must not appear in holder echo, got %+v", holderTags)
	}
	// status/conflict path shows it (no self-filter).
	targetTags, err := s.ListAliveForTarget(ctx, domain.Canonical(lock.Target.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	if len(targetTags) != 1 {
		t.Fatalf("self-tag must appear in target list, got %+v", targetTags)
	}
}

func TestAck_Idempotent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, id, tcAlice); err != nil {
		t.Fatalf("first ack: %v", err)
	}
	if err := s.Ack(ctx, id, tcAlice); err != nil {
		t.Fatalf("second ack must be no-op: %v", err)
	}
}

func TestAck_Orphan_NoOp(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	if err := s.Ack(ctx, "t-deadbeef", tcAlice); err != nil {
		t.Fatalf("ack unknown id must be no-op, got %v", err)
	}
}

func TestAck_NotMine_Rejects(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, id, tcBob); !errEqualsTo(err, ErrTagNotMine) {
		t.Fatalf("want ErrTagNotMine, got %v", err)
	}
}

// TestAck_ClassifyIsTransactional_RaceWithReclaim pins the loto-3c7y fix: the
// 0-row-UPDATE classify path must read the SAME transactional snapshot as the
// UPDATE. We drive a concurrent mutation into the window between the UPDATE and
// the classifying SELECT (via the ackClassifyHook seam) that rewrites the tag's
// lock_owner_uuid to a different holder — exactly the reclaim+retag race the
// audit describes. The owner-of-record at UPDATE time is Alice and the tag is
// already acked, so the only correct result is idempotent nil. The pre-fix
// autocommit code lets the SELECT see Bob and misclassifies as ErrTagNotMine;
// an immediate-mode tx serializes the writer behind the held lock so the SELECT
// sees Alice. We assert deterministic nil across repeated runs.
func TestAck_ClassifyIsTransactional_RaceWithReclaim(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Alice legitimately acks first → the racing Ack below hits 0 rows and must
	// classify idempotently (already-acked → nil), not as not-mine.
	if err := s.Ack(ctx, id, tcAlice); err != nil {
		t.Fatalf("priming ack: %v", err)
	}

	orig := ackClassifyHook
	defer func() { ackClassifyHook = orig }()
	// raceDone is closed when the racing writer goroutine finishes; the test
	// body waits on it after each Ack so the writer never leaks across
	// iterations and hookErr is read with a happens-before edge.
	var raceDone chan struct{}
	var hookErr error
	// The hook runs after the 0-row UPDATE and before the classifying SELECT.
	// It opens a SEPARATE writer (s.db pool / fresh conn) and rewrites the
	// owner — the reclaim-by-another-owner mutation. With the classify path in
	// one immediate-mode tx, this writer blocks on the held write lock until
	// the Ack tx commits, so it can never poison the snapshot the SELECT reads.
	// Run it in a goroutine with a brief settle so the no-tx path (which does
	// NOT hold a write lock) actually loses the race and the SELECT sees Bob.
	ackClassifyHook = func() {
		raceDone = make(chan struct{})
		done := raceDone
		go func() {
			defer close(done)
			if _, e := s.db.ExecContext(ctx,
				`UPDATE tags SET lock_owner_uuid = ? WHERE id = ?`, tcBob, id); e != nil {
				hookErr = e
			}
		}()
		// Give the racing writer a chance to commit before the SELECT. Under
		// the immediate-mode tx fix it is blocked on the write lock, so this is
		// the writer waiting on us, not us waiting on it — the Ack proceeds and
		// commits, then the writer lands harmlessly after.
		select {
		case <-done:
		case <-time.After(50 * time.Millisecond):
		}
	}

	for i := range 5 {
		// Reset owner to Alice and re-ack-prime before each iteration so every
		// run exercises the same logical state (Alice owns, already acked).
		if _, err := s.db.ExecContext(ctx,
			`UPDATE tags SET lock_owner_uuid = ?, acked_at = ? WHERE id = ?`,
			tcAlice, time.Now().UnixNano(), id); err != nil {
			t.Fatalf("reset iter %d: %v", i, err)
		}
		if err := s.Ack(ctx, id, tcAlice); err != nil {
			t.Fatalf("iter %d: Ack must be idempotent nil under reclaim race, got %v", i, err)
		}
		// Drain the racing writer before the next reset so its delayed UPDATE
		// can't bleed into the following iteration and so hookErr is read safely.
		<-raceDone
		if hookErr != nil {
			t.Fatalf("iter %d: hook mutation failed: %v", i, hookErr)
		}
	}
}

func TestOrphanFilter_OnLockDeletion(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: "doomed",
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a break: delete the host lock row directly.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ?`, lock.Target.Canonical); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAliveForOwner(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("orphan must be filtered, got %+v", got)
	}
	gotTarget, err := s.ListAliveForTarget(ctx, domain.Canonical(lock.Target.Canonical))
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTarget) != 0 {
		t.Fatalf("orphan must be filtered from target list, got %+v", gotTarget)
	}
}

func TestReleaseLocks_AcksTagsOnReleasedLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReleaseLocks(ctx, []domain.Target{lock.Target}, tcAlice); err != nil {
		t.Fatalf("ReleaseLocks: %v", err)
	}
	// Tag row should still exist with acked_at set (audit), not orphaned.
	var acked *int64
	var ackedNull sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT acked_at FROM tags WHERE id = ?`, id).Scan(&ackedNull); err != nil {
		t.Fatalf("tag row missing after release: %v", err)
	}
	if !ackedNull.Valid {
		t.Fatalf("release should ack tag, acked_at still NULL")
	}
	v := ackedNull.Int64
	acked = &v
	if *acked == 0 {
		t.Fatalf("acked_at = 0, want a real timestamp")
	}
}

// TestBreakLocks_GCsOrphanedTags asserts the break path reclaims the tags it
// orphans, in its own tx, rather than leaving them to accumulate until an
// operator runs `doctor --repair` (loto-qg0r). Break does NOT ack tags (a
// broken peer never read its notes — that distinction vs release-ack still
// holds); it hard-deletes them via gcTagsTx so retention is bounded on the hot
// path.
func TestBreakLocks_GCsOrphanedTags(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	id, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: tcPing,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Force-break by a 3rd party (bob).
	live := func(string, int, int64) bool { return true }
	res, err := s.BreakLocks(ctx, []domain.Target{lock.Target}, tcBob, BreakForce, "break", "h", live)
	if err != nil || res[0].Err != nil {
		t.Fatalf("break: %v / %v", err, res[0].Err)
	}
	// The orphaned tag must be reclaimed by the break tx (no row left behind,
	// no doctor --repair required).
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if n != 0 {
		t.Fatalf("break must gc the orphaned tag in its own tx; tag %s still present", id)
	}
}

func TestReleaseLocks_MultiTarget_AcksEachLocksTags(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	la, lockANs := acquireForTest(t, s, tcAGo, tcAlice)
	lb, lockBNs := acquireForTest(t, s, "b.go", tcAlice)
	idA, _ := s.InsertTag(ctx, NewTag{TargetCanonical: domain.Canonical(la.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockANs, TaggerUUID: tcBob, Text: "a"})
	idB, _ := s.InsertTag(ctx, NewTag{TargetCanonical: domain.Canonical(lb.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockBNs, TaggerUUID: tcBob, Text: "b"})
	if _, err := s.ReleaseLocks(ctx, []domain.Target{la.Target, lb.Target}, tcAlice); err != nil {
		t.Fatalf("multi release: %v", err)
	}
	for _, id := range []string{idA, idB} {
		var acked sql.NullInt64
		if err := s.db.QueryRowContext(ctx, `SELECT acked_at FROM tags WHERE id = ?`, id).Scan(&acked); err != nil {
			t.Fatalf("tag %s missing: %v", id, err)
		}
		if !acked.Valid {
			t.Fatalf("tag %s should be acked", id)
		}
	}
}

func TestDoctorRepair_GCsOrphanedTags(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: domain.Canonical(lock.Target.Canonical), LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: "doomed",
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate break-by-third-party: delete the host lock row directly.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ?`, lock.Target.Canonical); err != nil {
		t.Fatal(err)
	}
	if n := rawTagRowCount(t, s); n != 1 {
		t.Fatalf("precondition: 1 orphan tag row, got %d", n)
	}
	live := func(string, int, int64) bool { return true }
	if err := s.DoctorRepair(ctx, "h", tcAlice, live); err != nil {
		t.Fatalf("DoctorRepair: %v", err)
	}
	if n := rawTagRowCount(t, s); n != 0 {
		t.Fatalf("orphan tag should be GC'd, got %d rows", n)
	}
}

func rawTagRowCount(t *testing.T, s *Store) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tags`).Scan(&n); err != nil {
		t.Fatalf("count tags: %v", err)
	}
	return n
}

func errEqualsTo(got, want error) bool {
	return errors.Is(got, want)
}
