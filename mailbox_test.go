//go:build unix

package loto

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestMailboxAppendCompactRace stresses the append/compact critical section.
// Without the per-mailbox lock (loto-616), parallel appends could be lost
// when compactFile rewrote a snapshot taken pre-write. With the lock, every
// successful append must be readable after the run, regardless of how many
// compactions interleaved.
func TestMailboxAppendCompactRace(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "race-target.go")

	const writers = 8
	const perWriter = 60 // 8*60 = 480 > mailboxCompactAt (200) — guarantees compactions

	var wg sync.WaitGroup
	wg.Add(writers + 1)

	for w := range writers {
		go func() {
			defer wg.Done()
			for i := range perWriter {
				body := fmt.Sprintf("w=%d i=%d", w, i)
				if err := l.SendMsg(target, fmt.Sprintf("writer-%d", w), "@all", body, false); err != nil {
					t.Errorf("SendMsg w=%d i=%d: %v", w, i, err)
					return
				}
			}
		}()
	}

	// Concurrent compactor — racing the writers, exercising the rewrite path.
	go func() {
		defer wg.Done()
		for range 20 {
			if err := l.CompactMsgs(target); err != nil {
				t.Errorf("CompactMsgs: %v", err)
				return
			}
		}
	}()

	wg.Wait()

	msgs, err := l.ReadMsgs(target, "any-reader")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}

	seen := make(map[string]bool, writers*perWriter)
	for _, m := range msgs {
		if !strings.HasPrefix(m.Body, "w=") {
			continue
		}
		seen[m.Body] = true
	}

	missing := 0
	for w := range writers {
		for i := range perWriter {
			key := fmt.Sprintf("w=%d i=%d", w, i)
			if !seen[key] {
				missing++
				if missing <= 5 {
					t.Errorf("missing message: %s", key)
				}
			}
		}
	}
	if missing > 5 {
		t.Errorf("...and %d more missing", missing-5)
	}
	if missing == 0 {
		t.Logf("all %d messages survived %d concurrent compactions", writers*perWriter, 20)
	}
}

// TestSendMsgWithIsIdempotent verifies that re-appending a Msg with the same
// MsgID is a no-op — the dedupe-on-append guard for retry paths (loto-lgk.7).
func TestSendMsgWithIsIdempotent(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "idempotent.go")

	msg := Msg{
		MsgID: "fixed-id-abc",
		From:  "writer", To: "@all", Body: "hello",
	}
	for range 5 {
		if err := l.SendMsgWith(target, msg); err != nil {
			t.Fatalf("SendMsgWith: %v", err)
		}
	}
	got, err := l.ReadMsgs(target, "any-reader")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 message after 5 retries with same MsgID, got %d", len(got))
	}
	if got[0].MsgID != "fixed-id-abc" {
		t.Errorf("MsgID not preserved: %q", got[0].MsgID)
	}
}

// TestSendMsgAutoAssignsUniqueIDs verifies that SendMsg generates a fresh
// UUIDv4 per call so two consecutive sends are NOT collapsed.
func TestSendMsgAutoAssignsUniqueIDs(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "unique.go")

	for i := range 3 {
		if err := l.SendMsg(target, "w", "@all", fmt.Sprintf("m%d", i), false); err != nil {
			t.Fatalf("SendMsg %d: %v", i, err)
		}
	}
	got, err := l.ReadMsgs(target, "any-reader")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 distinct messages, got %d", len(got))
	}
	ids := make(map[string]bool)
	for _, m := range got {
		if m.MsgID == "" {
			t.Errorf("message missing MsgID: %+v", m)
		}
		if ids[m.MsgID] {
			t.Errorf("duplicate MsgID generated: %s", m.MsgID)
		}
		ids[m.MsgID] = true
	}
}

// TestCompactDedupesByMsgID: if duplicate-MsgID rows somehow land in the file
// (e.g. concurrent writers that bypassed dedupe, or schema rollback), compact
// must collapse them to one. Legacy rows without MsgID pass through unchanged.
func TestCompactDedupesByMsgID(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "compact-dedupe.go")

	// Seed via the public API so msgsPath/dirs are created.
	if err := l.SendMsg(target, "w", "@all", "seed", false); err != nil {
		t.Fatalf("SendMsg seed: %v", err)
	}
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		t.Fatalf("msgsPath: %v", err)
	}

	// Hand-write a mailbox with two rows sharing one MsgID, plus a legacy row.
	lines := []string{
		`{"msg_id":"dup-1","from":"w","to":"@all","body":"first","timestamp":"2026-04-29T12:00:00Z"}`,
		`{"msg_id":"dup-1","from":"w","to":"@all","body":"second","timestamp":"2026-04-29T12:00:01Z"}`,
		`{"from":"w","to":"@all","body":"legacy-no-id","timestamp":"2026-04-29T12:00:02Z"}`,
		`{"from":"w","to":"@all","body":"legacy-also-no-id","timestamp":"2026-04-29T12:00:03Z"}`,
	}
	if err := os.WriteFile(msgsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("seed write: %v", err)
	}

	if err := l.CompactMsgs(target); err != nil {
		t.Fatalf("CompactMsgs: %v", err)
	}
	got, err := l.ReadMsgs(target, "any-reader")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}

	bodies := make(map[string]int, len(got))
	for _, m := range got {
		bodies[m.Body]++
	}
	if bodies["first"] != 1 || bodies["second"] != 0 {
		t.Errorf("dup-1: want first=1 second=0, got first=%d second=%d", bodies["first"], bodies["second"])
	}
	if bodies["legacy-no-id"] != 1 || bodies["legacy-also-no-id"] != 1 {
		t.Errorf("legacy rows must pass through: %v", bodies)
	}
}

// TestReadStampsReadAtForDirectMessages: a recipient's first ReadMsgs stamps
// ReadAt on direct messages addressed to them; @all messages are never
// stamped; subsequent reads do not change ReadAt (idempotent).
func TestReadStampsReadAtForDirectMessages(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "ack.go")

	if err := l.SendMsgWith(target, Msg{From: "alice", To: "bob", Body: "direct", AckRequired: true}); err != nil {
		t.Fatalf("SendMsgWith direct: %v", err)
	}
	if err := l.SendMsg(target, "alice", "@all", "broadcast", false); err != nil {
		t.Fatalf("SendMsg broadcast: %v", err)
	}

	got, err := l.ReadMsgs(target, "bob")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got))
	}

	var direct, broadcast *Msg
	for i := range got {
		switch got[i].Body {
		case "direct":
			direct = &got[i]
		case "broadcast":
			broadcast = &got[i]
		}
	}
	if direct == nil || direct.ReadAt == nil {
		t.Fatalf("direct message must be stamped with ReadAt; got %+v", direct)
	}
	if broadcast == nil || broadcast.ReadAt != nil {
		t.Errorf("@all message must not be stamped; got %+v", broadcast)
	}

	first := *direct.ReadAt
	got2, err := l.ReadMsgs(target, "bob")
	if err != nil {
		t.Fatalf("ReadMsgs second: %v", err)
	}
	for _, m := range got2 {
		if m.Body == "direct" {
			if m.ReadAt == nil || !m.ReadAt.Equal(first) {
				t.Errorf("ReadAt must be stable across reads: first=%v second=%v", first, m.ReadAt)
			}
		}
	}
}

// TestReadDoesNotStampForOtherRecipient: a non-recipient reading the mailbox
// must not stamp ReadAt on someone else's direct message.
func TestReadDoesNotStampForOtherRecipient(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "no-stamp.go")

	if err := l.SendMsgWith(target, Msg{From: "alice", To: "bob", Body: "for-bob"}); err != nil {
		t.Fatalf("SendMsgWith: %v", err)
	}
	// Carol reads — she's not bob; the message isn't even returned to her,
	// and stamping must not occur.
	if _, err := l.ReadMsgs(target, "carol"); err != nil {
		t.Fatalf("ReadMsgs carol: %v", err)
	}
	got, err := l.ReadMsgs(target, "bob")
	if err != nil {
		t.Fatalf("ReadMsgs bob: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	if got[0].ReadAt == nil {
		t.Errorf("bob's read should stamp; got nil")
	}
}

// TestReadMsgsQuarantinesCorruptLines: a mailbox file with one valid + one
// truncated JSON line must (a) still return the valid message and (b)
// quarantine the corrupt line to a .corrupt sidecar — never silently drop it.
// Regression for loto-mi1.
func TestReadMsgsQuarantinesCorruptLines(t *testing.T) {
	l := newTestLOTO(t)
	target := filepath.Join(t.TempDir(), "mailbox-corrupt.go")

	// Trigger path creation by sending one valid message through the public API.
	if err := l.SendMsg(target, "writer", "@all", "good", false); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	msgsPath, err := l.msgsPath(target)
	if err != nil {
		t.Fatalf("msgsPath: %v", err)
	}

	// Append a truncated JSON line directly — simulates partial-write corruption.
	f, err := os.OpenFile(msgsPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open mailbox: %v", err)
	}
	if _, err := f.WriteString("{\"from\":\"x\",\"to\":\"@all\",\"body\":\"truncated\n"); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Append a second valid line through the public API so we can confirm
	// corruption doesn't poison downstream reads.
	if err := l.SendMsg(target, "writer", "@all", "after-corrupt", false); err != nil {
		t.Fatalf("SendMsg post-corrupt: %v", err)
	}

	msgs, err := l.ReadMsgs(target, "any-reader")
	if err != nil {
		t.Fatalf("ReadMsgs: %v", err)
	}
	bodies := make(map[string]bool, len(msgs))
	for _, m := range msgs {
		bodies[m.Body] = true
	}
	if !bodies["good"] {
		t.Errorf("expected valid message %q to survive corruption; got %+v", "good", msgs)
	}
	if !bodies["after-corrupt"] {
		t.Errorf("expected post-corruption message to be returned; got %+v", msgs)
	}

	// Corrupt sidecar must exist and contain the bad line — operator-visible.
	corruptPath := msgsPath + ".corrupt"
	data, err := os.ReadFile(corruptPath)
	if err != nil {
		t.Fatalf("expected quarantine file %s: %v", corruptPath, err)
	}
	if !strings.Contains(string(data), "truncated") {
		t.Errorf("quarantine file missing corrupt content: %q", data)
	}
}
