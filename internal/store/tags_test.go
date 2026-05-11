package store

import (
	"context"
	"testing"
	"time"

	"loto/internal/domain"
)

func mkTag(target, author, addressee, intent string) domain.TagRecord {
	tgt, _ := domain.Canonicalize(target)
	return domain.TagRecord{
		Target: tgt, Kind: domain.TagNote,
		AuthorUUID: author, AddresseeUUID: addressee, Intent: intent,
		CreatedAt: time.Now(),
	}
}

func TestAddAndListTags(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()

	tg := mkTag("a.go", "alice", "bob", "ping me")
	id, err := s.AddTag(ctx, tg)
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("AddTag must return id")
	}
	got, err := s.TagsOnTarget(ctx, tg.Target)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Intent != "ping me" {
		t.Fatalf("expected 1 tag with intent 'ping me', got %+v", got)
	}
}

func TestUntagAuthorOnly(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tg := mkTag("a.go", "alice", "", "note")
	id, _ := s.AddTag(ctx, tg)
	if err := s.RemoveTag(ctx, tg.Target, id, "bob"); err == nil {
		t.Fatal("non-author untag must fail")
	}
	if err := s.RemoveTag(ctx, tg.Target, id, "alice"); err != nil {
		t.Fatalf("author untag: %v", err)
	}
}

func TestUnreadCursor(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt, _ := domain.Canonicalize("a.go")
	tg1 := mkTag("a.go", "alice", "bob", "first")
	tg2 := mkTag("a.go", "alice", "bob", "second")
	tg2.CreatedAt = tg1.CreatedAt.Add(time.Second)
	if _, err := s.AddTag(ctx, tg1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTag(ctx, tg2); err != nil {
		t.Fatal(err)
	}

	unread, err := s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 2 {
		t.Fatalf("expected 2 unread, got %d", len(unread))
	}
	if err := s.MarkRead(ctx, "bob", tgt, tg2.CreatedAt); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if len(unread) != 0 {
		t.Fatalf("expected 0 unread after mark, got %d", len(unread))
	}
}

// TestMarkReadPreservesConcurrentInsert simulates gh#47: a tag inserted
// between display and MarkRead must NOT be marked read just because its
// created_at happens to be the MAX. Caller passes the upper bound that
// matches what was actually displayed.
func TestMarkReadPreservesConcurrentInsert(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt, _ := domain.Canonicalize("a.go")

	tg1 := mkTag("a.go", "alice", "bob", "shown")
	if _, err := s.AddTag(ctx, tg1); err != nil {
		t.Fatal(err)
	}

	unread, err := s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 1 {
		t.Fatalf("expected 1 unread before race, got %d", len(unread))
	}
	displayedUpTo := unread[0].CreatedAt

	// Race: a second tag lands after display, before MarkRead.
	tg2 := mkTag("a.go", "alice", "bob", "raced-in")
	tg2.CreatedAt = displayedUpTo.Add(time.Second)
	if _, err := s.AddTag(ctx, tg2); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkRead(ctx, "bob", tgt, displayedUpTo); err != nil {
		t.Fatal(err)
	}

	after, err := s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("raced-in tag must still be unread; got %d unread", len(after))
	}
	if after[0].Intent != "raced-in" {
		t.Fatalf("wrong tag survived: %q", after[0].Intent)
	}
}

// TestMarkReadNeverRegressesCursor — a stale MarkRead with an earlier
// upTo must not reopen tags that were already marked read by a later call.
func TestMarkReadNeverRegressesCursor(t *testing.T) {
	s := mustOpen(t)
	ctx := context.Background()
	tgt, _ := domain.Canonicalize("a.go")

	tg1 := mkTag("a.go", "alice", "bob", "first")
	tg2 := mkTag("a.go", "alice", "bob", "second")
	tg2.CreatedAt = tg1.CreatedAt.Add(time.Hour)
	if _, err := s.AddTag(ctx, tg1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTag(ctx, tg2); err != nil {
		t.Fatal(err)
	}

	if err := s.MarkRead(ctx, "bob", tgt, tg2.CreatedAt); err != nil {
		t.Fatal(err)
	}
	// Stale follow-up: earlier upTo. Cursor must not move backward.
	if err := s.MarkRead(ctx, "bob", tgt, tg1.CreatedAt); err != nil {
		t.Fatal(err)
	}
	unread, _ := s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if len(unread) != 0 {
		t.Fatalf("cursor regressed: %d unread", len(unread))
	}
}
