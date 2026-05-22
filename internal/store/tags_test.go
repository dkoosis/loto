package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestSchemaVersionPaired pins the const/PRAGMA pair so a future bump of one
// without the other fails fast instead of move-aside'ing every DB on open.
func TestSchemaVersionPaired(t *testing.T) {
	raw, err := os.ReadFile("schema.sql")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("PRAGMA user_version = %d;", schemaUserVersion)
	if !strings.Contains(string(raw), want) {
		t.Fatalf("schema.sql missing %q; schema.sql and store.go schemaUserVersion drifted", want)
	}
}

// acquireForTest acquires a lock for the test and returns its CreatedAt (ns).
func acquireForTest(t *testing.T, s *Store, name, agent string) (domain.LockRecord, int64) {
	t.Helper()
	rec := mkFileLock(t, name, agent, time.Hour)
	live := func(string, int) bool { return true }
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
		TargetCanonical: lock.Target.Canonical,
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            "why?",
	})
	if err != nil {
		t.Fatalf("InsertTag: %v", err)
	}
	got, err := s.ListAliveForHolder(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != id || got[0].Text != "why?" {
		t.Fatalf("ListAliveForHolder: %+v", got)
	}
}

func TestInsertTag_CapEnforcedTransactionally(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)

	for i := range tagCap {
		if _, err := s.InsertTag(ctx, NewTag{
			TargetCanonical: lock.Target.Canonical,
			LockOwnerUUID:   tcAlice,
			LockCreatedAt:   lockNs,
			TaggerUUID:      tcBob,
			Text:            fmt.Sprintf("note %d", i),
		}); err != nil {
			t.Fatalf("InsertTag %d: %v", i, err)
		}
	}
	_, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: lock.Target.Canonical,
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcBob,
		Text:            "overflow",
	})
	if !errEqualsTo(err, ErrTagCapReached) {
		t.Fatalf("want ErrTagCapReached, got %v", err)
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
		TargetCanonical: lock.Target.Canonical,
		LockOwnerUUID:   tcAlice,
		LockCreatedAt:   lockNs,
		TaggerUUID:      tcAlice,
		Text:            "self note",
	}); err != nil {
		t.Fatalf("InsertTag self: %v", err)
	}
	// Holder echo path suppresses it.
	holderTags, err := s.ListAliveForHolder(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(holderTags) != 0 {
		t.Fatalf("self-tag must not appear in holder echo, got %+v", holderTags)
	}
	// status/conflict path shows it (no self-filter).
	targetTags, err := s.ListAliveForTarget(ctx, lock.Target.Canonical)
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
		TargetCanonical: lock.Target.Canonical, LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: "ping",
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
		TargetCanonical: lock.Target.Canonical, LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: "ping",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ack(ctx, id, tcBob); !errEqualsTo(err, ErrTagNotMine) {
		t.Fatalf("want ErrTagNotMine, got %v", err)
	}
}

func TestOrphanFilter_OnLockDeletion(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	lock, lockNs := acquireForTest(t, s, tcAGo, tcAlice)
	if _, err := s.InsertTag(ctx, NewTag{
		TargetCanonical: lock.Target.Canonical, LockOwnerUUID: tcAlice, LockCreatedAt: lockNs,
		TaggerUUID: tcBob, Text: "doomed",
	}); err != nil {
		t.Fatal(err)
	}
	// Simulate a break: delete the host lock row directly.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ?`, lock.Target.Canonical); err != nil {
		t.Fatal(err)
	}
	got, err := s.ListAliveForHolder(ctx, tcAlice)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("orphan must be filtered, got %+v", got)
	}
	gotTarget, err := s.ListAliveForTarget(ctx, lock.Target.Canonical)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotTarget) != 0 {
		t.Fatalf("orphan must be filtered from target list, got %+v", gotTarget)
	}
}

func errEqualsTo(got, want error) bool {
	if got == nil || want == nil {
		return got == want
	}
	return got == want || got.Error() == want.Error()
}
