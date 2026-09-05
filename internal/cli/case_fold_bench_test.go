package cli

// Batch-resolution cost of the case fold (loto-f8m8). The cached/uncached pair
// is the evidence for caseCache existing: measured on APFS, 300 targets in one
// 300-entry directory run 94ms uncached and 0.7ms cached, and the uncached
// shape grows quadratically with the batch.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func benchPaths(tb testing.TB, n int) (string, []string) {
	tb.Helper()
	repo := tb.TempDir()
	dir := filepath.Join(repo, "internal", "store")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	names := make([]string, 0, n)
	for i := range n {
		f := fmt.Sprintf("file%03d.go", i)
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			tb.Fatal(err)
		}
		names = append(names, "internal/store/"+f)
	}
	return repo, names
}

func BenchmarkResolveDiskCaseUncached(b *testing.B) {
	repo, names := benchPaths(b, 300)
	b.ResetTimer()
	for range b.N {
		for _, n := range names {
			resolveDiskCase(nil, repo, n)
		}
	}
}

func BenchmarkResolveDiskCaseCached(b *testing.B) {
	repo, names := benchPaths(b, 300)
	b.ResetTimer()
	for range b.N {
		cc := newCaseCache()
		for _, n := range names {
			resolveDiskCase(cc, repo, n)
		}
	}
}
