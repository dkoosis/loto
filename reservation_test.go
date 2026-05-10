//go:build unix

package loto

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestReservationConcurrentRefreshAndList exercises the gh#19 / loto-77q
// race: a refresh-write can land between a reader's ReadFile (which sees
// expired bytes) and that reader's lazy-GC os.Remove — silently destroying
// the fresh reservation. With the per-pattern flock + bytes-Equal recheck
// in place, the fresh tag must always survive.
func TestReservationConcurrentRefreshAndList(t *testing.T) {
	l := newTestLOTO(t)
	pattern := "internal/store/**"

	// Seed a long-lived (non-expiring, very-long TTL) reservation so each
	// refresh writes fresh non-expired bytes; readers concurrently iterate.
	if _, err := l.Reserve("agent-x", "seed", pattern, time.Hour); err != nil {
		t.Fatalf("seed reserve: %v", err)
	}

	const refreshers = 8
	const listers = 8
	const iters = 50
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var refreshed atomic.Int64
	var listed atomic.Int64

	for range refreshers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				if _, err := l.Reserve("agent-x", "refresh", pattern, time.Hour); err != nil {
					t.Errorf("refresh: %v", err)
					return
				}
				refreshed.Add(1)
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}

	for range listers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range iters {
				if _, err := l.ListReservations(); err != nil {
					t.Errorf("list: %v", err)
					return
				}
				listed.Add(1)
				select {
				case <-stop:
					return
				default:
				}
			}
		}()
	}

	wg.Wait()
	close(stop)

	// After all goroutines finish, the reservation tag must still be on
	// disk — no stale reader's GC may have deleted the live refreshed entry.
	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("final list: %v", err)
	}
	if len(got) != 1 || got[0].Pattern != pattern {
		t.Fatalf("expected the %q reservation to survive %d refreshes / %d lists, got %v",
			pattern, refreshed.Load(), listed.Load(), got)
	}
}

// TestReservationLazyGCRemovesTrulyExpired confirms the lazy-GC path is
// still wired: an expired tag with no concurrent rewriter is unlinked on
// the first read.
func TestReservationLazyGCRemovesTrulyExpired(t *testing.T) {
	l := newTestLOTO(t)
	pattern := "tmp/**"

	if _, err := l.Reserve("agent-y", "short", pattern, 10*time.Millisecond); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	tagPath := filepath.Join(l.reservationsDir(), hashPattern(pattern)+reservationExt)
	time.Sleep(40 * time.Millisecond)

	got, err := l.ListReservations()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected expired reservation pruned, got %v", got)
	}
	if _, err := os.Stat(tagPath); !os.IsNotExist(err) {
		t.Errorf("expected tag file removed, stat err=%v", err)
	}
}
