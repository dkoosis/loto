package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

// The events CHECK constraint has to admit staged_lock_gate_fired — the
// advisory-first firing counter behind `loto check --held` (loto-7oik) — or
// the counter is silently unwritable and the evidence the rollout is
// gathering never exists.

func TestEventsCheck_AdmitsStagedGateFired(t *testing.T) {
	ctx := context.Background()
	s, err := OpenContext(ctx, filepath.Join(t.TempDir(), "loto.db"))
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer s.Close()

	var ddl string
	if err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		t.Fatalf("read events ddl: %v", err)
	}
	if !strings.Contains(ddl, "'staged_lock_gate_fired'") {
		t.Errorf("events CHECK must allow 'staged_lock_gate_fired', got: %s", ddl)
	}
	// ‡ The regression ensureEventsDetail's own comment warns about: an
	// events REBUILD that copies a column list without detail drops the
	// column. A fresh DB must still carry it after every ensure has run.
	if !strings.Contains(ddl, "detail") {
		t.Errorf("events rebuild dropped the detail column: %s", ddl)
	}

	if _, err := s.AppendEvent(ctx, domain.Event{
		Kind: EventStagedGateFired, ActorUUID: tcAlice,
		Reason: "warn", Detail: `{"mode":"warn","unheld":1}`, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("AppendEvent(%s): %v", EventStagedGateFired, err)
	}

	evs, err := s.ListEvents(ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := 0
	for i := range evs {
		if evs[i].Kind != EventStagedGateFired {
			continue
		}
		found++
		if evs[i].Detail != `{"mode":"warn","unheld":1}` {
			t.Errorf("detail payload not round-tripped: %q", evs[i].Detail)
		}
		if evs[i].Target.Canonical != "" {
			t.Errorf("a firing is commit-scoped, not per-path: target=%q", evs[i].Target.Canonical)
		}
	}
	if found != 1 {
		t.Errorf("want 1 firing row, got %d", found)
	}
}

// The migration on a DB whose events table predates the new kind: the rows
// already there survive WITH their detail payloads, and the new kind becomes
// insertable.
func TestEnsureEventsCheckStagedGate_PreservesDetailOnRebuild(t *testing.T) {
	ctx := context.Background()
	s, err := OpenContext(ctx, filepath.Join(t.TempDir(), "loto.db"))
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer s.Close()

	// Revert to the pre-loto-7oik events shape: the older CHECK list, with
	// the detail column ensureEventsDetail had already added.
	const revert = `
DROP TABLE events;
CREATE TABLE events (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded','lock_refreshed','gate_bypass','candidate_accepted','candidate_rejected')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  detail           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
INSERT INTO events (id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, detail, created_at)
VALUES ('e1','a.go','lock_acquired','alice','','because','{"kept":true}',1);`
	if _, err := s.db.ExecContext(ctx, revert); err != nil {
		t.Fatalf("revert events table: %v", err)
	}

	pending, err := ensureEventsCheckAllKinds(ctx, s.db, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if !pending {
		t.Fatal("the older events shape must read as pending")
	}
	if _, err := ensureEventsCheckAllKinds(ctx, s.db, true); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var detail string
	if err := s.db.QueryRowContext(ctx, `SELECT detail FROM events WHERE id='e1'`).Scan(&detail); err != nil {
		t.Fatalf("read migrated row: %v", err)
	}
	if detail != `{"kept":true}` {
		t.Errorf("rebuild lost the detail payload: %q", detail)
	}
	if _, err := s.AppendEvent(ctx, domain.Event{
		Kind: EventStagedGateFired, ActorUUID: tcAlice, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("new kind must insert after the rebuild: %v", err)
	}
	if pending, err := ensureEventsCheckAllKinds(ctx, s.db, false); err != nil || pending {
		t.Errorf("must be a no-op after applying: pending=%v err=%v", pending, err)
	}
}

// TestEnsureEventsCheckAllKinds_ReachesAnUpgradedDB is loto-qrgg's regression:
// the one-file extension contract event_kinds.go promises has to hold for
// databases that ALREADY EXIST, not just for fresh ones.
//
// The probe used to be the single newest kind, so it short-circuited on every
// DB that already had staged_lock_gate_fired — kind thirteen would render into
// schema.sql for fresh installs and never reach an upgraded one, whose old
// CHECK then rejected the first write of it at runtime. No test caught that,
// because tests open fresh DBs.
//
// Appending to allEventKinds here reproduces the upgrade exactly: schemaSQL is
// substituted once at package init, so it is already frozen WITHOUT the new
// kind by the time this runs. The DB this test opens therefore starts with the
// old CHECK and only the migration can widen it — which is the production
// shape, not a simulation of it.
func TestEnsureEventsCheckAllKinds_ReachesAnUpgradedDB(t *testing.T) {
	const futureKind = "future_kind_added_after_this_db_existed"
	orig := allEventKinds
	allEventKinds = append(append([]string{}, orig...), futureKind)
	t.Cleanup(func() { allEventKinds = orig })

	ctx := context.Background()
	s, err := OpenContext(ctx, filepath.Join(t.TempDir(), "loto.db"))
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer s.Close()

	var ddl string
	if err := s.db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		t.Fatalf("read events ddl: %v", err)
	}
	if !strings.Contains(ddl, "'"+futureKind+"'") {
		t.Fatalf("migrate must widen the CHECK to the newly declared kind: %s", ddl)
	}
	if _, err := s.AppendEvent(ctx, domain.Event{
		Kind: futureKind, ActorUUID: tcAlice, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("a kind appended to allEventKinds must be insertable after migrate: %v", err)
	}
	if pending, err := ensureEventsCheckAllKinds(ctx, s.db, false); err != nil || pending {
		t.Errorf("must be a no-op once every declared kind is admitted: pending=%v err=%v", pending, err)
	}
}
