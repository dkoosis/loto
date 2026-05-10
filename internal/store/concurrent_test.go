package store

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"loto/internal/domain"
)

// TestConcurrentOverlappingAcquire — spec acceptance item 8: N goroutines hit
// overlapping AcquireLock simultaneously through a countdown barrier; exactly
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
	tgt, _ := domain.Canonicalize("internal/store/store.go")
	live := func(string, int) bool { return true }
	now := time.Now()

	var ready, start sync.WaitGroup
	ready.Add(N)
	start.Add(1)

	var wins, conflicts, other atomic.Int32
	var done sync.WaitGroup
	done.Add(N)

	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer done.Done()
			ready.Done()
			start.Wait()
			rec := domain.LockRecord{
				Target:      tgt,
				OwnerUUID:   makeUUID(i),
				SessionUUID: makeUUID(i),
				Intent:      "race",
				CreatedAt:   now,
				ExpiresAt:   now.Add(time.Hour),
				Host:        "test",
				PID:         1000 + i,
			}
			_, err := s.AcquireLock(context.Background(), rec, live)
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
	var ce *ConflictError
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
