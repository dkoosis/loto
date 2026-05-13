package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// Concurrent first-Open: N goroutines hit Open() on a freshly-empty state
// dir. All must succeed; the op-flock ensures exactly one initializes the
// canonical schema and the rest reuse it.
func TestOpen_ConcurrentFirstOpen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "loto.db")

	const N = 16
	var ready, start sync.WaitGroup
	ready.Add(N)
	start.Add(1)

	var ok, fail atomic.Int32
	stores := make([]*Store, N)
	var done sync.WaitGroup
	done.Add(N)

	for i := range N {
		go func(idx int) {
			defer done.Done()
			ready.Done()
			start.Wait()
			s, err := Open(dbPath)
			if err != nil {
				t.Logf("goroutine %d: Open err: %v", idx, err)
				fail.Add(1)
				return
			}
			stores[idx] = s
			ok.Add(1)
		}(i)
	}
	ready.Wait()
	start.Done()
	done.Wait()

	for _, s := range stores {
		if s != nil {
			_ = s.Close()
		}
	}

	if ok.Load() != N {
		t.Fatalf("ok=%d (want %d), fail=%d", ok.Load(), N, fail.Load())
	}
}
