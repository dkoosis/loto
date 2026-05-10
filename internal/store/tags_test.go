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
	if err := s.MarkRead(ctx, "bob", tgt); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.UnreadTagsForAddressee(ctx, "bob", tgt)
	if len(unread) != 0 {
		t.Fatalf("expected 0 unread after mark, got %d", len(unread))
	}
}
