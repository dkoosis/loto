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
