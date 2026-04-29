//go:build unix

package loto

import (
	"fmt"
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
