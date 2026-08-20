package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
)

const (
	ttPrefix = "internal/store"
	ttText   = "loto-abc: rebase before you touch claims.go"
)

// mkNote builds a NewTerritoryTag with a live TTL. Prefix and tagger vary; the
// text does not, because no test here turns on it.
func mkNote(prefix, tagger string, ttl time.Duration) NewTerritoryTag {
	return NewTerritoryTag{
		PathPrefix: prefix,
		TaggerUUID: tagger,
		Text:       ttText,
		ExpiresAt:  time.Now().Add(ttl).UnixNano(),
	}
}

// TestInsertTerritoryTagNeedsNoHostLock is the whole feature in one assertion:
// `loto tag` used to refuse any path nobody held, which left "leave word for an
// agent that is not running yet" unserved once mail retired.
func TestInsertTerritoryTagNeedsNoHostLock(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	id, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour))
	if err != nil {
		t.Fatalf("InsertTerritoryTag with no host lock: %v", err)
	}
	if !strings.HasPrefix(id, "tt-") {
		t.Errorf("id must carry the tt- prefix so ack can route on it, got %q", id)
	}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].PathPrefix != ttPrefix || notes[0].TaggerUUID != tcAlice {
		t.Fatalf("want one note at %s by %s, got %+v", ttPrefix, tcAlice, notes)
	}
	if !notes[0].Live(time.Now().UnixNano()) {
		t.Error("a freshly written note must be live")
	}
}

// TestTerritoryTagSurvivesDoctorRepair is the regression that justifies the
// separate table. The `tags` table's orphan sweep is
// `DELETE ... WHERE NOT EXISTS (SELECT 1 FROM locks ...)`, which every hostless
// row matches by definition — folding these rows back into `tags` would make
// the first repair silently eat them. That is data loss, not invisibility.
func TestTerritoryTagSurvivesDoctorRepair(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	id, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	// A stale lock elsewhere, so repair has real work and walks every sweep path.
	stale := mkFileLock(t, "elsewhere.go", tcBob, -time.Hour)
	if _, err := s.AcquireLocks(ctx, []domain.LockRecord{stale}, liveProbe); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatal(err)
	}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || notes[0].ID != id {
		t.Fatalf("doctor --repair must not touch a live territory tag, got %+v", notes)
	}
}

func TestTerritoryTagPerPrefixCap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	var first string
	for i := range territoryTagPrefixCap {
		id, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour))
		if err != nil {
			t.Fatalf("note %d: %v", i, err)
		}
		if i == 0 {
			first = id
		}
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour)); !errors.Is(err, ErrTagCapReached) {
		t.Fatalf("want ErrTagCapReached past the per-prefix cap, got %v", err)
	}
	// Acking frees a slot: the cap counts LIVE notes, not rows ever written.
	if err := s.AckTerritoryTag(ctx, first, tcBob); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour)); err != nil {
		t.Errorf("acking must free a slot, got %v", err)
	}
}

// The per-prefix cap alone lets a loop bomb 50 sibling prefixes and call it 50
// different territories. The repo-wide cap is the other half of the guarantee.
func TestTerritoryTagRepoCap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	for i := range territoryTagRepoCap {
		p := ttPrefix + string(rune('a'+i%26)) + string(rune('a'+i/26))
		if _, err := s.InsertTerritoryTag(ctx, mkNote(p, tcAlice, time.Hour)); err != nil {
			t.Fatalf("note %d at %s: %v", i, p, err)
		}
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote("some/other/ground", tcAlice, time.Hour)); !errors.Is(err, ErrTagCapReached) {
		t.Fatalf("want ErrTagCapReached past the repo-wide cap, got %v", err)
	}
}

func TestTerritoryTagTextCap(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	n := mkNote(ttPrefix, tcAlice, time.Hour)
	n.Text = strings.Repeat("x", tagTextMaxBytes+1)
	if _, err := s.InsertTerritoryTag(ctx, n); !errors.Is(err, ErrTagTextTooLong) {
		t.Fatalf("want ErrTagTextTooLong, got %v", err)
	}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 0 {
		t.Errorf("a rejected write must leave nothing behind, got %+v", notes)
	}
}

// The 30d ceiling is the anti-inbox guarantee as a system property rather than
// caller discipline: no note outlives a month, whatever the caller passes.
func TestTerritoryTagTTLCeiling(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if _, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, territoryTagMaxTTL+time.Hour)); !errors.Is(err, ErrTerritoryTagTTLTooLong) {
		t.Fatalf("want ErrTerritoryTagTTLTooLong, got %v", err)
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, territoryTagMaxTTL-time.Hour)); err != nil {
		t.Errorf("a TTL inside the ceiling must be accepted, got %v", err)
	}
}

// ListClaims' contract, inherited: the store reports what is written and the
// caller decides what is worth showing.
func TestListTerritoryTagsReturnsExpired(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if _, err := s.InsertTerritoryTag(ctx, mkNote("b/two", tcAlice, -time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote("a/one", tcAlice, time.Hour)); err != nil {
		t.Fatal(err)
	}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("expired rows must come back, got %d", len(notes))
	}
	if notes[0].PathPrefix != "a/one" || notes[1].PathPrefix != "b/two" {
		t.Errorf("want prefix-ascending order, got %s then %s", notes[0].PathPrefix, notes[1].PathPrefix)
	}
}

// D4 encoded as a test: a later "only the holder may ack" tightening shows up
// red here. The agent who reads a note and decides NOT to take the territory is
// exactly the reader it served.
func TestAckTerritoryTagByAnyAgent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	id, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AckTerritoryTag(ctx, id, tcCarol); err != nil {
		t.Fatalf("any agent must be able to ack: %v", err)
	}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if notes[0].AckedAt == nil || notes[0].AckedByUUID != tcCarol {
		t.Fatalf("ack must record who dismissed it, got %+v", notes[0])
	}
	if notes[0].Live(time.Now().UnixNano()) {
		t.Error("an acked note must stop being live")
	}
}

func TestAckTerritoryTagIdempotent(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	if err := s.AckTerritoryTag(ctx, "tt-nosuchid", tcBob); err != nil {
		t.Errorf("acking an unknown id must be a no-op, got %v", err)
	}
	id, err := s.InsertTerritoryTag(ctx, mkNote(ttPrefix, tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AckTerritoryTag(ctx, id, tcBob); err != nil {
		t.Fatal(err)
	}
	notes, _ := s.ListTerritoryTags(ctx)
	firstAck := *notes[0].AckedAt
	if err := s.AckTerritoryTag(ctx, id, tcCarol); err != nil {
		t.Fatal(err)
	}
	notes, _ = s.ListTerritoryTags(ctx)
	if *notes[0].AckedAt != firstAck || notes[0].AckedByUUID != tcBob {
		t.Errorf("a second ack must not rewrite the first, got %+v", notes[0])
	}
}

func TestGCTerritoryTagsSweepsExpiredAndOldAcked(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	live, err := s.InsertTerritoryTag(ctx, mkNote("keep/live", tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertTerritoryTag(ctx, mkNote("drop/expired", tcAlice, -time.Minute)); err != nil {
		t.Fatal(err)
	}
	freshAck, err := s.InsertTerritoryTag(ctx, mkNote("keep/acked-recently", tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	oldAck, err := s.InsertTerritoryTag(ctx, mkNote("drop/acked-long-ago", tcAlice, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AckTerritoryTag(ctx, freshAck, tcBob); err != nil {
		t.Fatal(err)
	}
	if err := s.AckTerritoryTag(ctx, oldAck, tcBob); err != nil {
		t.Fatal(err)
	}
	// Backdate the old ack past the retention window.
	if _, err := s.db.ExecContext(ctx, `UPDATE territory_tags SET acked_at = ? WHERE id = ?`,
		time.Now().Add(-tagsRetentionAge-time.Hour).UnixNano(), oldAck); err != nil {
		t.Fatal(err)
	}

	if err := s.DoctorRepair(ctx, "doctor", deadProbe); err != nil {
		t.Fatal(err)
	}
	kept := map[string]bool{}
	notes, err := s.ListTerritoryTags(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notes {
		kept[n.ID] = true
	}
	if !kept[live] || !kept[freshAck] {
		t.Errorf("live and recently-acked notes must survive, kept=%v", kept)
	}
	if len(notes) != 2 {
		t.Errorf("expired and long-acked notes must be swept, got %+v", notes)
	}
}

// Only the genuinely undelivered ones. A note that was read and acked, then
// aged out, was delivered; reporting it would bury the real findings.
func TestExpiredTerritoryTagsReportsUnackedOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	unread, err := s.InsertTerritoryTag(ctx, mkNote("a/unread", tcAlice, -time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	read, err := s.InsertTerritoryTag(ctx, mkNote("b/read", tcAlice, -time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AckTerritoryTag(ctx, read, tcBob); err != nil {
		t.Fatal(err)
	}

	got, err := s.ExpiredTerritoryTags(ctx, time.Now().UnixNano())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != unread {
		t.Fatalf("want only the note that expired unread, got %+v", got)
	}
}
