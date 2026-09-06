package store

// Release-by-name across the key fold (loto-8soe, review of #310).
//
// The CLI mints every key folded on a case-folding filesystem. A row an OLDER
// loto wrote carries the on-disk spelling, and the in-memory predicates keep it
// BLOCKING — so if the SQL lookups stayed byte-exact it could never be released
// by name: `loto unlock foo.go` printed state=no-lock, exit 0, and left the row
// refusing every peer until its TTL.
//
// ‡ These tests do not probe the filesystem. WithCaseFoldedKeys is a flag the
// CLI sets from its own probe, so both branches run on every runner — the
// darwin leg is not what makes the folded case reachable.

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

// seedLegacyLock writes one lock row in the spelling given, through a store
// that does NOT fold — exactly what a pre-fold loto left behind — and returns
// the db path so a second Open can read it back under the new rule.
func seedLegacyLock(t *testing.T, l domain.LockRecord) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "loto.db")
	s, err := OpenContext(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{l}, aliveOn(tcHost)); err != nil {
		t.Fatalf("seed legacy lock: %v", err)
	}
	s.Close()
	return dbPath
}

func reopen(t *testing.T, dbPath string, fold bool) *Store {
	t.Helper()
	s, err := OpenContext(context.Background(), dbPath, WithCaseFoldedKeys(fold))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// TestReleaseFindsLegacyMixedCaseRowByFoldedName is the review's P1: a store
// that folds must reach the legacy row under the folded name, and one that does
// not must still treat the two spellings as two files.
func TestReleaseFindsLegacyMixedCaseRowByFoldedName(t *testing.T) {
	for _, tc := range []struct {
		name string
		fold bool
		want ReleaseOutcome
		left int // lock rows surviving the release
	}{
		{"case-folding-store", true, StateUnlocked, 0},
		{"byte-exact-store", false, StateNoLock, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			legacy := mkFileLock(t, "Foo.go", tcAlice, time.Hour)
			s := reopen(t, seedLegacyLock(t, legacy), tc.fold)

			// What the CLI hands the store now: the same file, folded key.
			folded := domain.Target{Canonical: strings.ToLower(legacy.Target.Canonical)}
			res, err := s.ReleaseLocks(ctx, []domain.Target{folded}, tcAlice, aliveOn(tcHost))
			if err != nil {
				t.Fatalf("ReleaseLocks: %v", err)
			}
			if res[0].State != tc.want {
				t.Errorf("release of %q against stored %q = %v; want %v",
					folded.Canonical, legacy.Target.Canonical, res[0].State, tc.want)
			}
			rows, err := s.ListLocks(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.left {
				t.Errorf("%d lock rows survive the release; want %d — a row that reports"+
					" released but stays in the table blocks every peer until its TTL", len(rows), tc.left)
			}
		})
	}
}

// TestLocksAtFindsLegacyMixedCaseRow: the same widening on the read side, which
// is what `loto status <path>` and tag delivery consult. A holder the gate
// refuses a peer over must be visible to the surfaces that explain the refusal.
func TestLocksAtFindsLegacyMixedCaseRow(t *testing.T) {
	ctx := context.Background()
	legacy := mkFileLock(t, "Foo.go", tcAlice, time.Hour)
	dbPath := seedLegacyLock(t, legacy)
	folded := domain.Target{Canonical: strings.ToLower(legacy.Target.Canonical)}

	if got, err := reopen(t, dbPath, true).LocksAt(ctx, folded); err != nil || len(got) != 1 {
		t.Errorf("folded LocksAt(%q) found %d holders (err %v); want 1", folded.Canonical, len(got), err)
	}
	if got, err := reopen(t, dbPath, false).LocksAt(ctx, folded); err != nil || len(got) != 0 {
		t.Errorf("byte-exact LocksAt(%q) found %d holders (err %v); want 0 — two files on a"+
			" case-sensitive filesystem", folded.Canonical, len(got), err)
	}
}

// TestUnclaimFindsLegacyMixedCasePrefix is the same defect on the claim
// surface: `loto unclaim docs` reported no-claim, exit 0, against a live legacy
// row spelled `Docs` — which went on covering every path beneath it until its
// TTL.
func TestUnclaimFindsLegacyMixedCasePrefix(t *testing.T) {
	for _, tc := range []struct {
		name string
		fold bool
		want ClaimReleaseState
		left int // claim rows surviving the release
	}{
		{"case-folding-store", true, ClaimStateReleased, 0},
		{"byte-exact-store", false, ClaimStateNoClaim, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "loto.db")
			now := time.Now()

			// A prefix an older loto reserved, in the on-disk spelling.
			legacy := domain.ClaimRecord{
				PathPrefix: "Docs", OwnerUUID: tcAlice, SessionUUID: tcAlice,
				Intent: tcTest, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
				Host: tcHost,
			}
			seed, err := OpenContext(ctx, dbPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := seed.ClaimPrefix(ctx, legacy, aliveOn(tcHost)); err != nil {
				t.Fatalf("seed legacy claim: %v", err)
			}
			seed.Close()

			s := reopen(t, dbPath, tc.fold)
			got, err := s.ReleaseClaim(ctx, "docs", tcAlice) // the folded prefix the CLI mints
			if err != nil {
				t.Fatalf("ReleaseClaim: %v", err)
			}
			if got.State != tc.want {
				t.Errorf("unclaim of %q against stored %q = %v; want %v",
					"docs", legacy.PathPrefix, got.State, tc.want)
			}
			rows, err := s.ListClaims(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != tc.left {
				t.Errorf("%d claim rows survive the unclaim; want %d — a claim that reports"+
					" released but stays in the table covers its whole subtree until its TTL",
					len(rows), tc.left)
			}
		})
	}
}
