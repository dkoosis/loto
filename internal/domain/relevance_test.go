package domain

import (
	"testing"
	"time"
)

func TestRelevantTagsForLockAcquire(t *testing.T) {
	target, _ := Canonicalize("a/b.go")
	other, _ := Canonicalize("c/d.go")
	now := time.Now()
	tags := []TagRecord{
		{ID: "t-1", Target: target, AddresseeUUID: tcAlice, CreatedAt: now},
		{ID: "t-2", Target: target, AddresseeUUID: "", CreatedAt: now},
		{ID: "t-3", Target: target, AddresseeUUID: "bob", CreatedAt: now},
		{ID: "t-4", Target: other, AddresseeUUID: tcAlice, CreatedAt: now},
		{ID: "t-5", Target: target, AddresseeUUID: tcAlice, ExpiresAt: ptrTime(now.Add(-1)), CreatedAt: now},
	}
	got := RelevantTags(tags, tcAlice, target, RelevanceLockAcquire, now, false)
	gotIDs := map[string]bool{}
	for _, tr := range got {
		gotIDs[tr.ID] = true
	}
	if !gotIDs["t-1"] || !gotIDs["t-2"] {
		t.Errorf("missing addressed/unaddressed tags: %v", gotIDs)
	}
	if gotIDs["t-3"] || gotIDs["t-4"] || gotIDs["t-5"] {
		t.Errorf("unexpected tags surfaced: %v", gotIDs)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }
