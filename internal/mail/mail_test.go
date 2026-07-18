package mail

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"loto/internal/domain"
)

const (
	tmUUIDAlice = "aaaaaaaa-1111-4111-8111-111111111111"
	tmUUIDBob   = "bbbbbbbb-2222-4222-8222-222222222222"
	tmUUIDCara  = "cccccccc-3333-4333-8333-333333333333"
	tmBody      = "loto-test: ping"
)

func openTestBox(t *testing.T) *Box {
	t.Helper()
	b, err := Open(context.Background(), filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func addrs(reader, slug string) []string {
	return []string{reader, domain.AddrAll, domain.RepoAddr(slug)}
}

// markInbox lists reader's inbox and marks exactly those IDs read — the
// production call sequence (display then mark the displayed set).
func markInbox(t *testing.T, b *Box, reader domain.AgentUUID, slug string) {
	t.Helper()
	msgs, err := b.Inbox(context.Background(), reader, addrs(string(reader), slug))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(msgs))
	for i := range msgs {
		ids[i] = msgs[i].ID
	}
	if err := b.MarkRead(context.Background(), reader, ids); err != nil {
		t.Fatal(err)
	}
}

func send(t *testing.T, b *Box, from, to, body string) string {
	t.Helper()
	// FromHandle left empty: Summary falls back to the short UUID, keeping
	// per-sender identity distinct without inventing handles per test.
	id, err := b.Send(context.Background(), domain.Message{
		FromUUID: domain.AgentUUID(from), To: to, Body: body,
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	return id
}

func TestSendInboxDirect(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	send(t, b, tmUUIDAlice, tmUUIDBob, tmBody)

	got, err := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Body != tmBody || got[0].FromUUID != tmUUIDAlice {
		t.Fatalf("bob inbox = %+v, want 1 msg from alice", got)
	}
	if got[0].ExpiresAt.Sub(got[0].CreatedAt) != DefaultTTL {
		t.Errorf("default TTL not applied: created=%v expires=%v", got[0].CreatedAt, got[0].ExpiresAt)
	}

	other, err := b.Inbox(ctx, tmUUIDCara, addrs(tmUUIDCara, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("cara should not see bob's direct mail: %+v", other)
	}
}

func TestBroadcastPerReaderRead(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	send(t, b, tmUUIDAlice, domain.AddrAll, tmBody)

	for _, reader := range []string{tmUUIDBob, tmUUIDCara} {
		got, err := b.Inbox(ctx, domain.AgentUUID(reader), addrs(reader, "loto"))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 {
			t.Fatalf("reader %s should see broadcast, got %d", reader, len(got))
		}
	}

	markInbox(t, b, tmUUIDBob, "loto")
	bobAfter, _ := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if len(bobAfter) != 0 {
		t.Fatalf("bob marked read but still sees %d", len(bobAfter))
	}
	caraAfter, _ := b.Inbox(ctx, tmUUIDCara, addrs(tmUUIDCara, "loto"))
	if len(caraAfter) != 1 {
		t.Fatalf("cara's read cursor must be independent of bob's, got %d", len(caraAfter))
	}
}

func TestSenderExcludedFromOwnBroadcast(t *testing.T) {
	b := openTestBox(t)
	send(t, b, tmUUIDAlice, domain.AddrAll, tmBody)
	got, err := b.Inbox(context.Background(), tmUUIDAlice, addrs(tmUUIDAlice, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("sender must not receive own broadcast: %+v", got)
	}
}

// Self-exclusion is scoped to broadcasts: direct mail an agent addresses to
// its own UUID (a cross-session note-to-self) must still arrive.
func TestSelfDirectMailDelivered(t *testing.T) {
	b := openTestBox(t)
	send(t, b, tmUUIDAlice, tmUUIDAlice, "loto-qhw: note to self")
	got, err := b.Inbox(context.Background(), tmUUIDAlice, addrs(tmUUIDAlice, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("self-addressed direct mail should arrive, got %d", len(got))
	}
}

// MarkRead marks only the IDs passed, so a message that arrives after the
// display query but before the mark is NOT consumed unseen.
func TestMarkReadOnlyConsumesGivenIDs(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	first := send(t, b, tmUUIDAlice, tmUUIDBob, "first")

	// Bob "displays" only the first message, then a second arrives.
	displayed := []string{first}
	send(t, b, tmUUIDAlice, tmUUIDBob, "second (arrived post-display)")

	if err := b.MarkRead(ctx, tmUUIDBob, displayed); err != nil {
		t.Fatal(err)
	}
	remaining, err := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].Body != "second (arrived post-display)" {
		t.Fatalf("post-display message must survive mark-read: %+v", remaining)
	}
}

// A repo whose slug is "all" must not collide with the @all broadcast.
func TestRepoAddrReservesAll(t *testing.T) {
	if got := domain.RepoAddr("all"); got == domain.AddrAll {
		t.Fatalf("RepoAddr(\"all\") = %q collides with broadcast %q", got, domain.AddrAll)
	}
	b := openTestBox(t)
	ctx := context.Background()
	// Broadcast, and repo-"all" mail, must land in disjoint channels.
	send(t, b, tmUUIDAlice, domain.AddrAll, "broadcast")
	send(t, b, tmUUIDAlice, domain.RepoAddr("all"), "repo-all only")

	// An agent NOT in repo "all" sees only the broadcast.
	elsewhere, _ := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if len(elsewhere) != 1 || elsewhere[0].Body != "broadcast" {
		t.Fatalf("agent outside repo-all should see only broadcast: %+v", elsewhere)
	}
	// An agent in repo "all" sees both (broadcast via @all, repo via @all-repo).
	inAll, _ := b.Inbox(ctx, tmUUIDCara, addrs(tmUUIDCara, "all"))
	if len(inAll) != 2 {
		t.Fatalf("agent in repo-all should see broadcast + repo mail, got %d", len(inAll))
	}
}

func TestRepoAddressRouting(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	send(t, b, tmUUIDAlice, domain.RepoAddr("loto"), tmBody)

	inLoto, _ := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if len(inLoto) != 1 {
		t.Fatalf("bob acting in loto should see @loto mail, got %d", len(inLoto))
	}
	inOther, _ := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "cc-plugins"))
	if len(inOther) != 0 {
		t.Fatalf("bob acting in cc-plugins should NOT see @loto mail, got %d", len(inOther))
	}
}

func TestExpiryFiltersAndPurges(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	now := time.Now()
	sendExpired := func() {
		if _, err := b.Send(ctx, domain.Message{
			FromUUID: tmUUIDAlice, To: tmUUIDBob, Body: tmBody,
			CreatedAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour),
		}); err != nil {
			t.Fatal(err)
		}
	}
	sendExpired()

	// Read never lists expired mail, regardless of purge.
	got, _ := b.Inbox(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if len(got) != 0 {
		t.Fatalf("expired mail listed: %+v", got)
	}

	// Reads do NOT purge (read-only Open stays read-only). The expired row is
	// still on disk after a read-only cycle.
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("read path must not purge; want 1 row on disk, got %d", n)
	}

	// A write (Send of a fresh live message) purges the expired row, leaving
	// only the live one.
	send(t, b, tmUUIDAlice, tmUUIDBob, "live")
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("write should purge expired rows; want 1 live row, got %d", n)
	}
}

func TestSendValidation(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	cases := []struct {
		name string
		msg  domain.Message
		want error
	}{
		{"empty body", domain.Message{FromUUID: tmUUIDAlice, To: tmUUIDBob, Body: "  "}, domain.ErrMsgBodyEmpty},
		{"oversized body", domain.Message{FromUUID: tmUUIDAlice, To: tmUUIDBob, Body: string(make([]byte, domain.MsgBodyMaxBytes+1))}, domain.ErrMsgBodyTooLarge},
		{"empty addr", domain.Message{FromUUID: tmUUIDAlice, Body: tmBody}, domain.ErrMsgBadAddr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := b.Send(ctx, tc.msg); !errors.Is(err, tc.want) {
				t.Errorf("Send = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSummaryDedupesSenders(t *testing.T) {
	b := openTestBox(t)
	ctx := context.Background()
	send(t, b, tmUUIDAlice, tmUUIDBob, "one")
	send(t, b, tmUUIDAlice, tmUUIDBob, "two")
	send(t, b, tmUUIDCara, tmUUIDBob, "three")

	n, senders, err := b.Summary(ctx, tmUUIDBob, addrs(tmUUIDBob, "loto"))
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	if len(senders) != 2 {
		t.Errorf("senders = %v, want 2 deduplicated", senders)
	}
}
