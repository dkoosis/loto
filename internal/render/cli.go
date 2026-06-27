// Package render formats CLI output per docs/design.md:
// triage count on the first body line, deterministic sort, key=value rows,
// no pluralized prose, no ANSI. Target paths are cwd-relative when possible —
// the store emits canonical (repo-relative) paths, but other surfaces may
// leak absolute paths; relToCwd handles both.
package render

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

// holderTag formats a UUID as "Handle(uuid-prefix)" when the agent record
// resolves on this host; otherwise returns the bare UUID. Surfaces a
// human-readable holder name in conflict reports (loto-b3o) without losing
// the stable UUID key for fleet automation.
func holderTag(uuid string) string {
	if a, err := identity.LookupByUUID(uuid); err == nil && a.Handle != "" {
		short := uuid
		if len(short) > 8 {
			short = short[:8]
		}
		return a.Handle + "(" + short + ")"
	}
	return uuid
}

// holderMemo caches holderTag results within a single render pass. Each
// EmitConflictWithTags / EmitTagFooter call resolves a given UUID once —
// identity.LookupByUUID does a ReadFile+Unmarshal, so without the memo a
// blocker list or tag footer sharing a holder UUID was an N+1 (loto-kyib).
// The zero value is usable; a nil resolve defaults to holderTag.
type holderMemo struct {
	cache   map[string]string
	resolve func(uuid string) string // overridable for tests; nil → holderTag
}

func (m *holderMemo) tag(uuid string) string {
	if v, ok := m.cache[uuid]; ok {
		return v
	}
	r := m.resolve
	if r == nil {
		r = holderTag
	}
	v := r(uuid)
	if m.cache == nil {
		m.cache = make(map[string]string)
	}
	m.cache[uuid] = v
	return v
}

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

// EmitLockSuccess renders the acquired-lock block. It takes records (not bare
// targets) so each row carries its mode (loto-k5el.2). Mode is normalized via
// EffectiveMode so a legacy/empty value renders as exclusive.
func EmitLockSuccess(w io.Writer, recs []domain.LockRecord) {
	cwd := getCwd()
	sorted := append([]domain.LockRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	fmt.Fprintf(w, "✓ locked count=%d\n", len(sorted))
	for i := range sorted {
		fmt.Fprintf(w, "✓ target=%s mode=%s\n", relToCwd(sorted[i].Target.Canonical, cwd), sorted[i].EffectiveMode())
	}
}

// EmitConflictWithTags renders the conflict block and, for each blocker,
// appends pending tags from tagsByTarget[canonical] as `ℹ tag …` rows beneath
// the `⚠ target=…` line. Pass nil to suppress tag surfacing.
func EmitConflictWithTags(w io.Writer, ce *store.MultiConflictError, tagsByTarget map[string][]store.Tag) {
	cwd := getCwd()
	holders := &holderMemo{}
	blockers := append([]domain.LockRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ target=%s blocker=%s intent=%q expires_at=%s\n",
			relToCwd(b.Target.Canonical, cwd), holders.tag(b.OwnerUUID), b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339))
		for _, t := range tagsByTarget[b.Target.Canonical] {
			emitTagRow(w, t, "  ", cwd, holders)
		}
	}
}

// EmitTagFooter renders the holder-facing trailing block of pending external
// tags. Empty input emits nothing — the caller's primary output must stand
// alone when there's no message to surface. Sort order is the caller's
// responsibility (store ListAlive* already orders by created_at ASC, id ASC).
func EmitTagFooter(w io.Writer, tags []store.Tag, ownerUUID string) {
	if len(tags) == 0 {
		return
	}
	cwd := getCwd()
	holders := &holderMemo{}
	fmt.Fprintf(w, "ℹ tags count=%d owner=%s\n", len(tags), holders.tag(ownerUUID))
	for _, t := range tags {
		emitTagRow(w, t, "", cwd, holders)
	}
}

// EmitTagRows renders just the per-tag lines (no count header). Use for inline
// blocks beneath a per-file status line where the surrounding context already
// names the target. Empty input emits nothing.
func EmitTagRows(w io.Writer, tags []store.Tag) {
	cwd := getCwd()
	holders := &holderMemo{}
	for _, t := range tags {
		emitTagRow(w, t, "  ", cwd, holders)
	}
}

// emitTagRow renders one tag line. cwd and holders are passed in so callers
// hoist the os.Getwd() syscall and identity lookups out of their loops
// (the file convention documented on relToCwd; loto-kyib).
func emitTagRow(w io.Writer, t store.Tag, indent, cwd string, holders *holderMemo) {
	at := time.Unix(0, t.CreatedAt).UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "ℹ %stag id=%s at=%s from=%s target=%s text=%q\n",
		indent, t.ID, at, holders.tag(t.TaggerUUID), relToCwd(t.TargetCanonical, cwd), t.Text)
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
	if len(results) == 0 {
		fmt.Fprintf(w, "ℹ no locks owned\n")
		return 0
	}
	cwd := getCwd()
	sorted := append([]store.ReleaseResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	// A restore-failed release deleted the lock row in-tx (locks_release.go) — a
	// successful unlock with a failed chmod restore — so it counts toward the
	// unlocked total. The restore failures surface as a distinct first-line field
	// (and per-row ⚠ lines below) so the Claude consumer sees both facts.
	successCount := 0
	restoreFailed := 0
	exit := 0
	for _, r := range sorted {
		switch r.State {
		case store.StateUnlocked:
			successCount++
		case store.StateRestoreFailed:
			successCount++
			restoreFailed++
			exit = 1
		case store.StateNotOwner:
			exit = 1
		case store.StateNoLock:
			// no-op: nothing was owned at this target, not a release.
		}
	}
	if restoreFailed > 0 {
		fmt.Fprintf(w, "✓ unlocked count=%d restore-failed=%d\n", successCount, restoreFailed)
	} else {
		fmt.Fprintf(w, "✓ unlocked count=%d\n", successCount)
	}
	for _, r := range sorted {
		path := relToCwd(r.Target.Canonical, cwd)
		switch r.State {
		case store.StateUnlocked:
			fmt.Fprintf(w, "✓ target=%s\n", path)
		case store.StateNoLock:
			fmt.Fprintf(w, "ℹ target=%s state=no-lock\n", path)
		case store.StateNotOwner:
			fmt.Fprintf(w, "✗ target=%s state=not-owner owner=%s\n", path, r.Owner)
		case store.StateRestoreFailed:
			writeRestoreFailed(w, "target", path, r.RestoreErr, r.AuditErr)
		}
	}
	return exit
}

// writeRestoreFailed renders a post-unlock write-mode restore failure. When the
// mode_restore_failed audit event was itself lost, it appends the audit-hole
// signal so the operator can re-emit or alert (gh#107). label distinguishes a
// plain unlock ("target") from a forced break ("broken target").
func writeRestoreFailed(w io.Writer, label, path string, restoreErr, auditErr error) {
	if auditErr != nil {
		fmt.Fprintf(w, "⚠ %s=%s state=restore-failed err=%q audit-hole=%q\n",
			label, path, errString(restoreErr), errString(auditErr))
		return
	}
	fmt.Fprintf(w, "⚠ %s=%s state=restore-failed err=%q\n", label, path, errString(restoreErr))
}

// EmitBreakResults renders per-target outcomes of `unlock --force` (BreakLocks).
// Clean breaks go to outW; problems — missing lock, authorize/break errors, a
// post-break write-mode restore failure, and a lost mode_restore_failed audit
// event — go to errW. Returns the suggested exit code. Surfaces RestoreErr and
// AuditErr (gh#107), which the prior inline renderer dropped: a forced break
// that left the file read-only, or whose audit event was lost, was silently
// reported as a clean "✓ broken".
func EmitBreakResults(outW, errW io.Writer, results []store.BreakResult) int {
	cwd := getCwd()
	exit := 0
	for _, r := range results {
		path := relToCwd(r.Target.Canonical, cwd)
		switch {
		case r.Err == nil && r.RestoreErr != nil:
			writeRestoreFailed(errW, "broken target", path, r.RestoreErr, r.AuditErr)
			if exit < 1 {
				exit = 1
			}
		case r.Err == nil:
			fmt.Fprintf(outW, "✓ broken target=%s\n", path)
		case errors.Is(r.Err, store.ErrNoLockAtTarget):
			fmt.Fprintf(errW, "✗ no lock at target=%s\n", path)
			if exit < 1 {
				exit = 1
			}
		default:
			fmt.Fprintf(errW, "✗ target=%s err=%v\n", path, r.Err)
			exit = 3
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
