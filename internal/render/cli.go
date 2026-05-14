// Package render formats CLI output per docs/design.md:
// triage count on the first body line, deterministic sort, key=value rows,
// no pluralized prose, no ANSI. All target paths printed cwd-relative when
// possible — absolute paths from the store are converted at the surface.
package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// relPath returns p relative to cwd if that's a clean descent; else p unchanged.
// Uses filepath.IsLocal (Go 1.20+) to test "doesn't escape cwd" — this avoids
// the strings.HasPrefix(rel, "..") false-positive on paths like "..foo/bar"
// (a legitimate descent into a dir whose name starts with two dots).
func relPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || !filepath.IsLocal(rel) {
		return p
	}
	return rel
}

func EmitLockSuccess(w io.Writer, targets []domain.Target) {
	sorted := append([]domain.Target(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Canonical < sorted[j].Canonical })
	fmt.Fprintf(w, "✓ locked count=%d\n", len(sorted))
	for _, t := range sorted {
		fmt.Fprintf(w, "✓ target=%s\n", relPath(t.Canonical))
	}
}

func EmitConflict(w io.Writer, ce *store.MultiConflictError) {
	blockers := append([]domain.LockRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ target=%s blocker=%s intent=%q expires_at=%s\n",
			relPath(b.Target.Canonical), b.OwnerUUID, b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

func EmitChmodFailure(w io.Writer, cfe *store.ChmodFailureError) {
	failed := 0
	for _, f := range cfe.Failures {
		if !f.RolledBack && f.Err != nil {
			failed++
		}
	}
	fmt.Fprintf(w, "✗ chmod-failed count=%d\n", failed)
	sorted := append([]store.ChmodFailure(nil), cfe.Failures...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	for _, f := range sorted {
		path := relPath(f.Target.Canonical)
		switch {
		case f.Err != nil && !f.RolledBack:
			fmt.Fprintf(w, "✗ target=%s err=%v rolled-back=no\n", path, f.Err)
		case f.Err != nil && f.RolledBack:
			fmt.Fprintf(w, "✗ target=%s err=%v rolled-back=yes\n", path, f.Err)
		default:
			fmt.Fprintf(w, "✓ target=%s state=restored\n", path)
		}
	}
}

// InvalidTarget describes a pre-store rejection (bad path, wrong kind, dup).
type InvalidTarget struct {
	Path   string
	Reason string // e.g. "not-regular-file", "not-found", "symlink", "duplicate-target", "stat-failed: ..."
}

func EmitInvalid(w io.Writer, items []InvalidTarget) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	fmt.Fprintf(w, "✗ invalid count=%d\n", len(items))
	for _, it := range items {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", relPath(it.Path), it.Reason)
	}
}

// EmitReleaseResults renders per-target outcomes and returns the suggested
// exit code: 0 if no not-owner / restore-failed rows, 1 otherwise.
// Renders canonical-sorted regardless of input order (caller passes input order;
// render owns deterministic output).
func EmitReleaseResults(w io.Writer, results []store.ReleaseResult) int {
	sorted := append([]store.ReleaseResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	successCount := 0
	exit := 0
	for _, r := range sorted {
		if r.State == store.StateUnlocked {
			successCount++
		}
		if r.State == store.StateNotOwner || r.State == store.StateRestoreFailed {
			exit = 1
		}
	}
	fmt.Fprintf(w, "✓ unlocked count=%d\n", successCount)
	for _, r := range sorted {
		path := relPath(r.Target.Canonical)
		switch r.State {
		case store.StateUnlocked:
			fmt.Fprintf(w, "✓ target=%s\n", path)
		case store.StateNoLock:
			fmt.Fprintf(w, "ℹ target=%s state=no-lock\n", path)
		case store.StateNotOwner:
			fmt.Fprintf(w, "✗ target=%s state=not-owner holder=%s\n", path, r.Holder)
		case store.StateRestoreFailed:
			fmt.Fprintf(w, "⚠ target=%s state=restore-failed err=%v\n", path, r.RestoreErr)
		}
	}
	return exit
}
