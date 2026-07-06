package domain

import (
	"testing"
	"time"
)

const tcStorePrefix = "internal/store"

func TestPrefixOverlaps(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		name string
	}{
		{tcStorePrefix, tcStorePrefix, true, "exact-eq"},
		{tcStorePrefix, "internal/store/sub", true, "ancestor"},
		{"internal/store/sub", tcStorePrefix, true, "descendant"},
		{tcStorePrefix, "internal/storefront", false, "sibling-string-prefix"},
		{"internal/storefront", tcStorePrefix, false, "sibling-string-prefix-rev"},
		{tcStorePrefix, "internal/render", false, "disjoint"},
		{"pkg/a", "pkg/a/deep", true, "deep-ancestor"},
		{"a", "a/b/c", true, "multi-segment-ancestor"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PrefixOverlaps(c.a, c.b); got != c.want {
				t.Errorf("PrefixOverlaps(%q,%q) = %v; want %v", c.a, c.b, got, c.want)
			}
			if got := PrefixOverlaps(c.b, c.a); got != c.want {
				t.Errorf("PrefixOverlaps(%q,%q) symmetry = %v; want %v", c.b, c.a, got, c.want)
			}
		})
	}
}

func TestClaimRecordExpired(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name      string
		expiresAt time.Time
		want      bool
	}{
		{"future", now.Add(time.Hour), false},
		{"past", now.Add(-time.Hour), true},
		{"exactly-now", now, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := ClaimRecord{PathPrefix: tcStorePrefix, ExpiresAt: c.expiresAt}
			if got := r.Expired(now); got != c.want {
				t.Errorf("Expired(now) with expires_at=%v = %v; want %v", c.expiresAt, got, c.want)
			}
		})
	}
}

func TestClaimCoversTarget(t *testing.T) {
	now := time.Now()
	const myUUID = "11111111-1111-1111-1111-111111111111"
	const foeUUID = "22222222-2222-2222-2222-222222222222"
	cases := []struct {
		name   string
		c      ClaimRecord
		target string
		want   bool
	}{
		{"foreign-live-covers", ClaimRecord{PathPrefix: tcStorePrefix, OwnerUUID: foeUUID, ExpiresAt: now.Add(time.Hour)}, tcStorePrefix + "/file.go", true},
		{"own-claim", ClaimRecord{PathPrefix: tcStorePrefix, OwnerUUID: myUUID, ExpiresAt: now.Add(time.Hour)}, tcStorePrefix + "/file.go", false},
		{"expired", ClaimRecord{PathPrefix: tcStorePrefix, OwnerUUID: foeUUID, ExpiresAt: now.Add(-time.Hour)}, tcStorePrefix + "/file.go", false},
		{"non-overlapping-sibling", ClaimRecord{PathPrefix: tcStorePrefix, OwnerUUID: foeUUID, ExpiresAt: now.Add(time.Hour)}, "internal/storefront/file.go", false},
		{"exact-prefix-match", ClaimRecord{PathPrefix: tcStorePrefix, OwnerUUID: foeUUID, ExpiresAt: now.Add(time.Hour)}, tcStorePrefix, true},
		{"strict-ancestor-deep", ClaimRecord{PathPrefix: "pkg", OwnerUUID: foeUUID, ExpiresAt: now.Add(time.Hour)}, "pkg/a/b/c/deep.go", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClaimCoversTarget(c.c, c.target, myUUID, now); got != c.want {
				t.Errorf("ClaimCoversTarget(%+v, %q) = %v; want %v", c.c, c.target, got, c.want)
			}
		})
	}
}

func TestSameCanonical(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
		name string
	}{
		{"a", "a", true, "exact-eq"},
		{"a", "b", false, "exact-diff"},
		{tcAxGo, tcAxGo, true, "nested-eq"},
		{tcAxGo, "b/x.go", false, "literal-disjoint"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ta, _ := Canonicalize(c.a)
			tb, _ := Canonicalize(c.b)
			if got := SameCanonical(ta, tb); got != c.want {
				t.Errorf("SameCanonical(%q,%q) = %v; want %v", c.a, c.b, got, c.want)
			}
			if got := SameCanonical(tb, ta); got != c.want {
				t.Errorf("SameCanonical(%q,%q) symmetry = %v; want %v", c.b, c.a, got, c.want)
			}
		})
	}
}
