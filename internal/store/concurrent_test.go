package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestConcurrentOverlappingAcquire — spec acceptance item 8: N goroutines hit
// overlapping AcquireLocks simultaneously through a countdown barrier; exactly
// one wins. SQLite WAL + _txlock=immediate + busy_timeout=5s should serialize
// writers cleanly.
func TestConcurrentOverlappingAcquire(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const N = 16
	target := filepath.Join(dir, "race.go")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tgt := domain.Target{Canonical: target}
	live := func(string, int, int64) bool { return true }
	now := time.Now()

	var ready, start sync.WaitGroup
	ready.Add(N)
	start.Add(1)

	var wins, conflicts, other atomic.Int32
	var done sync.WaitGroup
	done.Add(N)

	for i := range N {
		go func() {
			defer done.Done()
			ready.Done()
			start.Wait()
			rec := domain.LockRecord{
				Target:      tgt,
				OwnerUUID:   domain.AgentUUID(makeUUID(i)),
				SessionUUID: makeUUID(i),
				Intent:      "race",
				CreatedAt:   now,
				ExpiresAt:   now.Add(time.Hour),
				Host:        tcTest,
				PID:         1000 + i,
			}
			_, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live)
			switch {
			case err == nil:
				wins.Add(1)
			case isConflictErr(err):
				conflicts.Add(1)
			default:
				t.Logf("goroutine %d: unexpected err: %v", i, err)
				other.Add(1)
			}
		}()
	}
	ready.Wait()
	start.Done()
	done.Wait()

	if wins.Load() != 1 {
		t.Fatalf("wins=%d (want 1); conflicts=%d other=%d", wins.Load(), conflicts.Load(), other.Load())
	}
	if int(conflicts.Load()+other.Load()) != N-1 {
		t.Errorf("losers=%d (want %d); other=%d", conflicts.Load()+other.Load(), N-1, other.Load())
	}
}

func isConflictErr(err error) bool {
	var ce *MultiConflictError
	return errors.As(err, &ce)
}

func makeUUID(i int) string {
	return "00000000-0000-0000-0000-" + filepathHex(i)
}

func filepathHex(i int) string {
	const hex = "0123456789abcdef"
	out := make([]byte, 12)
	for j := 11; j >= 0; j-- {
		out[j] = hex[i&0xf]
		i >>= 4
	}
	return string(out)
}
