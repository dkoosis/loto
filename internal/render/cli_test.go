package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

const aGo = "a.go"

func TestEmitLockSuccess_SortedDeterministic(t *testing.T) {
	var buf bytes.Buffer
	EmitLockSuccess(&buf, []domain.Target{
		{Canonical: "z.go"},
		{Canonical: aGo},
	})
	got := buf.String()
	wantHead := "✓ locked count=2\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("first line want %q, got: %s", wantHead, got)
	}
	if strings.Index(got, "target=a.go") > strings.Index(got, "target=z.go") {
		t.Errorf("not sorted: %s", got)
	}
}

func TestEmitConflict_TriageFirst(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitConflict(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: aGo}, OwnerUUID: "Green", Intent: "x", ExpiresAt: now},
			{Target: domain.Target{Canonical: "c.go"}, OwnerUUID: "Red", Intent: "y", ExpiresAt: now},
		},
	})
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=2\n") {
		t.Errorf("triage first: %s", got)
	}
}

func TestEmitReleaseResults_MixedOutcomes(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: aGo}, State: store.StateUnlocked},
		{Target: domain.Target{Canonical: "b.go"}, State: store.StateNoLock},
		{Target: domain.Target{Canonical: "c.go"}, State: store.StateNotOwner, Holder: "BlueOak"},
	})
	if exit != 1 {
		t.Errorf("any not-owner → exit 1, got %d", exit)
	}
	got := buf.String()
	if !strings.Contains(got, "✓ unlocked count=1\n") {
		t.Errorf("triage count = successful releases only: %s", got)
	}
	if !strings.Contains(got, "state=no-lock") || !strings.Contains(got, "state=not-owner") {
		t.Errorf("missing distinct states: %s", got)
	}
	if !strings.Contains(got, "holder=BlueOak") {
		t.Errorf("missing holder: %s", got)
	}
}
