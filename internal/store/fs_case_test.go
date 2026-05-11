package store

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFSCaseProbeAndCache(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got1, err := s.FSCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	got2, err := s.FSCaseSensitive(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got1 != got2 {
		t.Errorf("cached value mismatch: %v vs %v", got1, got2)
	}
}

// TestFSCaseProbeConcurrent stresses the probeFSCase shared-filename race
// from gh#49: two probes running simultaneously could see each other's
// defer-cleanup mid-Stat and cache a wrong answer for the DB lifetime.
// With per-call unique probe filenames and INSERT OR IGNORE caching,
// concurrent callers must observe the same answer and leave no debris.
func TestFSCaseProbeConcurrent(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	const N = 16
	var wg sync.WaitGroup
	results := make([]bool, N)
	errs := make([]error, N)
	for i := range N {
		wg.Go(func() {
			results[i], errs[i] = s.FSCaseSensitive(dir)
		})
	}
	wg.Wait()

	for i := range N {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if results[i] != results[0] {
			t.Fatalf("disagreement: goroutine %d=%v vs 0=%v", i, results[i], results[0])
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".loto-case-probe") {
			t.Errorf("probe debris left behind: %s", e.Name())
		}
	}
}
