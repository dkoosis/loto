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
//
// loto-qev1: the create-race window is narrow, so a single 16-way burst
// only fails intermittently. Repeating the burst against a fresh temp dir
// each iteration makes the SQLITE_IOERR(1802) / SQLITE_BUSY / stale
// user_version race reliably reproducible in one `go test` run — and, post-
// fix, reliably green.
func TestOpen_ConcurrentFirstOpen(t *testing.T) {
	const iterations = 40
	for iter := range iterations {
		runConcurrentFirstOpenBurst(t, iter)
	}
}

// runConcurrentFirstOpenBurst races N goroutines on the first Open() of a
// fresh DB and asserts all succeed. A failure calls t.Fatalf, which ends the
// test (and the iteration loop) via runtime.Goexit.
func runConcurrentFirstOpenBurst(t *testing.T, iter int) {
	t.Helper()
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
				t.Logf("iter %d goroutine %d: Open err: %v", iter, idx, err)
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
		t.Fatalf("iter %d: ok=%d (want %d), fail=%d", iter, ok.Load(), N, fail.Load())
	}
}
