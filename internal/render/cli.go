// Package render formats CLI output per docs/design.md:
// triage count on the first body line, deterministic sort, key=value rows,
// no pluralized prose, no ANSI. Target paths are cwd-relative when possible —
// the store emits canonical (repo-relative) paths, but other surfaces may
// leak absolute paths; relToCwd handles both.
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

// relToCwd returns p relative to cwd when p is absolute and the relative form
// is a clean descent (doesn't escape cwd). Relative inputs are returned as-is,
// since the store enforces repo-relative canonical paths and any conversion
// requires absolute anchors that aren't available here.
//
// cwd is passed in so callers hoist the os.Getwd() syscall out of loops.
// An empty cwd disables conversion.
func relToCwd(p, cwd string) string {
	if cwd == "" || !filepath.IsAbs(p) {
		return p
	}
	rel, err := filepath.Rel(cwd, p)
	if err != nil || !filepath.IsLocal(rel) {
		return p
	}
	return rel
}

// getCwd returns the current working directory or "" on error.
// Render functions degrade gracefully (absolute paths just stay absolute).
func getCwd() string {
	cwd, err := os.Getwd()
	if err != nil {
		return ""
	}
	return cwd
}

func EmitLockSuccess(w io.Writer, targets []domain.Target) {
	cwd := getCwd()
	sorted := append([]domain.Target(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Canonical < sorted[j].Canonical })
	fmt.Fprintf(w, "✓ locked count=%d\n", len(sorted))
	for _, t := range sorted {
		fmt.Fprintf(w, "✓ target=%s\n", relToCwd(t.Canonical, cwd))
	}
}

func EmitConflict(w io.Writer, ce *store.MultiConflictError) {
	cwd := getCwd()
	blockers := append([]domain.LockRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ target=%s blocker=%s intent=%q expires_at=%s\n",
			relToCwd(b.Target.Canonical, cwd), b.OwnerUUID, b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

func EmitChmodFailure(w io.Writer, cfe *store.ChmodFailureError) {
	cwd := getCwd()
	failed := 0
	for _, f := range cfe.Failures {
		if f.Err != nil {
			failed++
		}
	}
	fmt.Fprintf(w, "✗ chmod-failed count=%d\n", failed)
	sorted := append([]store.ChmodFailure(nil), cfe.Failures...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	// store.rollbackStripped invariant: RolledBack=true ⟺ Err==nil.
	// So Err!=nil → rolled-back=no; Err==nil → state=restored.
	for _, f := range sorted {
		path := relToCwd(f.Target.Canonical, cwd)
		if f.Err != nil {
			fmt.Fprintf(w, "✗ target=%s err=%q rolled-back=no\n", path, f.Err.Error())
		} else {
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
	cwd := getCwd()
	sorted := append([]InvalidTarget(nil), items...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	fmt.Fprintf(w, "✗ invalid count=%d\n", len(sorted))
	for _, it := range sorted {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", relToCwd(it.Path, cwd), it.Reason)
	}
}

// EmitReleaseResults renders per-target outcomes and returns the suggested
// exit code: 0 if no not-owner / restore-failed rows, 1 otherwise.
// Renders canonical-sorted regardless of input order (caller passes input order;
// render owns deterministic output).
func EmitReleaseResults(w io.Writer, results []store.ReleaseResult) int {
	cwd := getCwd()
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
		path := relToCwd(r.Target.Canonical, cwd)
		switch r.State {
		case store.StateUnlocked:
			fmt.Fprintf(w, "✓ target=%s\n", path)
		case store.StateNoLock:
			fmt.Fprintf(w, "ℹ target=%s state=no-lock\n", path)
		case store.StateNotOwner:
			fmt.Fprintf(w, "✗ target=%s state=not-owner holder=%s\n", path, r.Holder)
		case store.StateRestoreFailed:
			fmt.Fprintf(w, "⚠ target=%s state=restore-failed err=%q\n", path, errString(r.RestoreErr))
		}
	}
	return exit
}

func errString(e error) string {
	if e == nil {
		return ""
	}
	return e.Error()
}
