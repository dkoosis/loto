//go:build unix

package loto

import (
	"sort"
	"testing"
	"time"
)

func TestBackfillIncludesHeldReservedAndMsgs(t *testing.T) {
	l := newTestLOTO(t)
	tmp := t.TempDir() + "/a.go"
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("setup: %v", err)
		}
	}

	lock, err := l.TryFileLock("agent-1", "edit a", tmp)
	must(err)
	t.Cleanup(func() { _ = lock.Unlock() })

	_, err = l.Reserve("agent-2", "scan store", "internal/store/**", time.Hour)
	must(err)

	must(l.SendMsg(tmp, "agent-3", "agent-1", "hello", false))

	since := time.Now().Add(-time.Minute)
	events, err := l.Backfill(since)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}

	kinds := map[EventKind]int{}
	for _, e := range events {
		kinds[e.Kind]++
	}
	if kinds[EventHeld] == 0 {
		t.Errorf("missing held event; got %+v", events)
	}
	if kinds[EventReserved] == 0 {
		t.Errorf("missing reserved event; got %+v", events)
	}
	if kinds[EventMsg] == 0 {
		t.Errorf("missing msg event; got %+v", events)
	}

	if !sort.SliceIsSorted(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) }) {
		t.Errorf("events not sorted by time: %+v", events)
	}
}

func TestBackfillRespectsSinceCutoff(t *testing.T) {
	l := newTestLOTO(t)
	tmp := t.TempDir() + "/a.go"
	lock, err := l.TryFileLock("agent-1", "edit", tmp)
	if err != nil {
		t.Fatalf("TryFileLock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Unlock() })

	future := time.Now().Add(time.Hour)
	events, err := l.Backfill(future)
	if err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	for _, e := range events {
		if e.Kind == EventHeld {
			t.Errorf("expected no held events past since cutoff, got %+v", e)
		}
	}
}

func TestWatchEmitsHeldAndReleased(t *testing.T) {
	l := newTestLOTO(t)
	ctx := t.Context()

	ch, err := l.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	tmp := t.TempDir() + "/a.go"
	lock, err := l.TryFileLock("agent-x", "edit a", tmp)
	if err != nil {
		t.Fatalf("TryFileLock: %v", err)
	}

	got := drainEvents(ch, 2*time.Second, func(evs []Event) bool {
		for _, e := range evs {
			if e.Kind == EventHeld && e.Agent == "agent-x" {
				return true
			}
		}
		return false
	})
	if !containsKind(got, EventHeld) {
		t.Fatalf("did not observe held event; got %+v", got)
	}

	if err := lock.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}

	got = drainEvents(ch, 2*time.Second, func(evs []Event) bool {
		return containsKind(evs, EventReleased)
	})
	if !containsKind(got, EventReleased) {
		t.Fatalf("did not observe released event; got %+v", got)
	}
}

func TestWatchEmitsReservedAndUnreserved(t *testing.T) {
	l := newTestLOTO(t)
	ctx := t.Context()

	ch, err := l.Watch(ctx)
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}

	if _, err := l.Reserve("agent-r", "intent", "internal/**", time.Hour); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	got := drainEvents(ch, 2*time.Second, func(evs []Event) bool {
		return containsKind(evs, EventReserved)
	})
	if !containsKind(got, EventReserved) {
		t.Fatalf("did not observe reserved event; got %+v", got)
	}

	if err := l.Unreserve("internal/**"); err != nil {
		t.Fatalf("Unreserve: %v", err)
	}
	got = drainEvents(ch, 2*time.Second, func(evs []Event) bool {
		return containsKind(evs, EventUnreserved)
	})
	if !containsKind(got, EventUnreserved) {
		t.Fatalf("did not observe unreserved event; got %+v", got)
	}
}

func drainEvents(ch <-chan Event, timeout time.Duration, until func([]Event) bool) []Event {
	deadline := time.After(timeout)
	var collected []Event
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return collected
			}
			collected = append(collected, ev)
			if until != nil && until(collected) {
				return collected
			}
		case <-deadline:
			return collected
		}
	}
}

func containsKind(evs []Event, k EventKind) bool {
	for _, e := range evs {
		if e.Kind == k {
			return true
		}
	}
	return false
}
