package store

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEventKinds_AllDeclaredConstantsInAllEventKinds is the mechanical guard
// the bead's Rule promises (loto-123y): a Go event-kind constant that exists
// but was never appended to allEventKinds — the single site the schema CHECK
// list and the current (ensureEventsCheckStagedGate) rebuild DDL both render
// from — fails here, not at a runtime CHECK-constraint violation on someone's
// first write of the new kind.
//
// It parses event_kinds.go directly (go/parser, not reflection) so the
// comparison is against what a human actually TYPED as a constant, not
// against allEventKinds itself — checking a derived list against its own
// source would never catch an entry someone forgot to add to that source.
func TestEventKinds_AllDeclaredConstantsInAllEventKinds(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(".", "event_kinds.go"), nil, 0)
	if err != nil {
		t.Fatalf("parse event_kinds.go: %v", err)
	}

	declared := map[string]string{} // const name -> string value
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					continue
				}
				declared[name.Name] = val
			}
		}
	}
	if len(declared) == 0 {
		t.Fatal("parsed zero constants from event_kinds.go — parser or path is broken")
	}

	inList := make(map[string]bool, len(allEventKinds))
	for _, k := range allEventKinds {
		inList[k] = true
	}
	for name, val := range declared {
		if !inList[val] {
			t.Errorf("%s = %q is declared in event_kinds.go but missing from allEventKinds — "+
				"the schema CHECK list and the current rebuild DDL will never admit it", name, val)
		}
	}
}

// TestEventKindCheckSQL_FreshSchemaAdmitsEveryDeclaredKind proves the
// runtime-substitution mechanism actually wires up: a freshly opened DB's
// events.event_kind CHECK — built by replacing schema.sql's
// eventKindCheckPlaceholder with eventKindCheckSQL() — names every kind in
// allEventKinds, and every one of them is actually insertable.
func TestEventKindCheckSQL_FreshSchemaAdmitsEveryDeclaredKind(t *testing.T) {
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
	if strings.Contains(ddl, eventKindCheckPlaceholder) {
		t.Fatal("the placeholder was never substituted — schemaSQL is unrendered")
	}
	for _, k := range allEventKinds {
		if !strings.Contains(ddl, "'"+k+"'") {
			t.Errorf("fresh-DB events CHECK missing declared kind %q: %s", k, ddl)
		}
	}

	for i, k := range allEventKinds {
		id := "ek-" + strconv.Itoa(i)
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO events (id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, detail, created_at)
			 VALUES (?, '', ?, 'tester', NULL, '', '', 1)`, id, k); err != nil {
			t.Errorf("insert kind %q: %v", k, err)
		}
	}
}

// TestMigrate_LegacyEventsDetailSurvivesFullMigrate is the end-to-end version
// of the AC "existing event rows in a legacy database still migrate and read
// back with detail preserved": rather than calling ensureEventsCheckStagedGate
// directly (as TestEnsureEventsCheckStagedGate_PreservesDetailOnRebuild
// does), it reverts a real, fully-current DB's events table to the pre-
// loto-7oik shape and drives the upgrade through s.migrate — the exact path
// re-Opening a legacy DB takes in production.
func TestMigrate_LegacyEventsDetailSurvivesFullMigrate(t *testing.T) {
	ctx := context.Background()
	s, err := OpenContext(ctx, filepath.Join(t.TempDir(), "loto.db"))
	if err != nil {
		t.Fatalf("OpenContext: %v", err)
	}
	defer s.Close()

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
VALUES ('e-legacy','a.go','candidate_rejected','alice','','because','{"created":[]}',1);`
	if _, err := s.db.ExecContext(ctx, revert); err != nil {
		t.Fatalf("revert events table to legacy shape: %v", err)
	}

	if err := s.migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var detail string
	if err := s.db.QueryRowContext(ctx,
		`SELECT detail FROM events WHERE id='e-legacy'`).Scan(&detail); err != nil {
		t.Fatalf("read migrated legacy row: %v", err)
	}
	if detail != `{"created":[]}` {
		t.Errorf("full migrate lost the legacy row's detail: %q", detail)
	}

	// And the kind added after this legacy shape was current is now writable.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO events (id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, detail, created_at)
		 VALUES ('e-new', '', ?, 'alice', NULL, '', '{"mode":"warn"}', 2)`,
		EventStagedGateFired); err != nil {
		t.Errorf("new kind must insert after full migrate: %v", err)
	}
}
