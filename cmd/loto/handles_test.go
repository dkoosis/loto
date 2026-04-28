package main

import (
	"crypto/rand"
	"fmt"
	"testing"
)

// TestHandleDeterminism verifies that the same UUID always produces the same handle.
func TestHandleDeterminism(t *testing.T) {
	uuids := []string{
		"a3f2b1c0-dead-beef-cafe-0123456789ab",
		"00000000-0000-0000-0000-000000000000",
		"ffffffff-ffff-ffff-ffff-ffffffffffff",
		newUUID(),
		newUUID(),
	}
	for _, uuid := range uuids {
		h1 := generateHandle(uuid)
		h2 := generateHandle(uuid)
		if h1 != h2 {
			t.Errorf("generateHandle(%q) not deterministic: %q != %q", uuid, h1, h2)
		}
		if h1 == "" {
			t.Errorf("generateHandle(%q) returned empty string", uuid)
		}
	}
}

// TestHandleFormat verifies handles are non-empty PascalCase strings.
func TestHandleFormat(t *testing.T) {
	for i := 0; i < 100; i++ {
		uuid := randomUUID()
		h := generateHandle(uuid)
		if len(h) < 4 {
			t.Errorf("handle too short: %q (uuid=%q)", h, uuid)
		}
		if h[0] < 'A' || h[0] > 'Z' {
			t.Errorf("handle does not start with uppercase: %q", h)
		}
	}
}

// TestHandleCollisionRate generates 10k handles from random UUIDs and asserts
// that no single handle dominates more than 1% of the space (i.e., no handle
// appears more than 100 times). This guards against degenerate distributions.
func TestHandleCollisionRate(t *testing.T) {
	const samples = 10_000
	counts := make(map[string]int, samples)
	for i := 0; i < samples; i++ {
		h := generateHandle(randomUUID())
		counts[h]++
	}
	for handle, count := range counts {
		if count > samples/100 { // >1% of samples
			t.Errorf("handle %q appears %d times (%.1f%%) — handle space too small or biased",
				handle, count, float64(count)/samples*100)
		}
	}
	t.Logf("10k samples → %d unique handles (%.1f%% unique)", len(counts), float64(len(counts))/samples*100)
}

// TestHandleSpaceSize documents the total combination count.
func TestHandleSpaceSize(t *testing.T) {
	total := len(adjectives) * len(animals)
	if total < 5_000 {
		t.Errorf("handle space too small: %d combinations (want ≥5000)", total)
	}
	t.Logf("handle space: %d adjectives × %d animals = %d combinations",
		len(adjectives), len(animals), total)
}

func randomUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
