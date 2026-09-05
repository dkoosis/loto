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
	"loto/internal/store"
)

// holderTag names a lock's holder in a report row. The owner id IS the
// Claude Code session id (loto-jnid), which is what ListAgents and
// SendMessage address, so it is printed verbatim — there is no friendlier
// name loto could resolve it to that the caller does not already have.
func holderTag(uuid string) string {
	return uuid
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

// EmitBeaconSuccess renders the minted-beacon block. Distinct from
// EmitLockSuccess on purpose: a beacon is machinery the gate minted, not a
// claim an agent made, and nothing will ever release it by hand — so the TTL
// is the one piece of state worth printing beside the path.
func EmitBeaconSuccess(w io.Writer, recs []domain.LockRecord, ttl time.Duration) {
	cwd := getCwd()
	sorted := append([]domain.LockRecord(nil), recs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	fmt.Fprintf(w, "✓ beacon count=%d ttl=%s\n", len(sorted), ttl)
	for i := range sorted {
		fmt.Fprintf(w, "✓ target=%s mode=%s\n", relToCwd(sorted[i].Target.Canonical, cwd), sorted[i].EffectiveMode())
	}
}

// EmitConflictWithTags renders the conflict block and, for each blocker,
// appends pending tags from tagsByTarget[canonical] as `ℹ tag …` rows beneath
// the `⚠ target=…` line. Pass nil to suppress tag surfacing.
func EmitConflictWithTags(w io.Writer, ce *store.MultiConflictError, tagsByTarget map[string][]store.Tag) {
	cwd := getCwd()
	blockers := append([]domain.LockRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		// branch= names the tree the holder took the lock from (loto-16cf) —
		// a foreign branch is the "their working tree is not yours" signal.
		// Omitted when unrecorded (pre-16cf rows, non-git acquire paths).
		branch := ""
		if b.Branch != "" {
			branch = " branch=" + b.Branch
		}
		fmt.Fprintf(w, "⚠ target=%s blocker=%s intent=%q expires_at=%s%s\n",
			relToCwd(b.Target.Canonical, cwd), holderTag(string(b.OwnerUUID)), b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339), branch)
		for _, t := range tagsByTarget[b.Target.Canonical] {
			emitTagRow(w, t, "  ", cwd)
		}
	}
}

// EmitCandidateClaimConflict renders the blocked-by-candidate-claim block
// (loto-u2p7): count-first, ⚠ per blocker naming the candidate id, its
// owning session, and its age — CandidateClaimConflictError's own Error()
// string ("candidate claim conflict: N blocker(s)") gives the operator
// nothing to act on, which was the bug this replaces at the `loto lock` call
// site. now is the caller's acquisition-attempt clock, so age is stable
// within one command run even across several blocker rows.
func EmitCandidateClaimConflict(w io.Writer, ce *store.CandidateClaimConflictError, now time.Time) {
	cwd := getCwd()
	blockers := append([]domain.CandidateClaim(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].PathCanonical != blockers[j].PathCanonical {
			return blockers[i].PathCanonical < blockers[j].PathCanonical
		}
		return blockers[i].CandidateID < blockers[j].CandidateID
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ target=%s candidate=%s session=%s age=%s\n",
			relToCwd(b.PathCanonical, cwd), b.CandidateID, holderTag(string(b.SessionUUID)),
			now.Sub(b.CreatedAt).Round(time.Second))
	}
}

// EmitClaimSuccess renders the acquired-claim block (loto-7af9). Single-claim
// by design — the claim verb takes exactly one prefix — so the count header is
// constant; it stays count-first for surface consistency with lock/unlock.
// The ttl shown is the record's own lease span (ExpiresAt−CreatedAt), so the
// row can never disagree with what was stored.
func EmitClaimSuccess(w io.Writer, rec domain.ClaimRecord) {
	fmt.Fprintf(w, "✓ claimed count=1\n")
	fmt.Fprintf(w, "✓ prefix=%s ttl=%s expires_at=%s\n",
		relToCwd(rec.PathPrefix, getCwd()), rec.ExpiresAt.Sub(rec.CreatedAt),
		rec.ExpiresAt.UTC().Format(time.RFC3339))
	// Advisory-limit reminder (pass-2 strategic-fit): a claim binds claimants
	// only — lock/check under the prefix do not consult it, so the per-file
	// lock discipline still applies inside claimed territory.
	fmt.Fprintf(w, "ℹ advisory: claim does not block lock/check under the prefix — still lock files before editing\n")
}

// EmitClaimConflict renders the blocked-claim block: count-first, ⚠ per
// blocker, holder named by owner id, sorted prefix then created_at.
func EmitClaimConflict(w io.Writer, ce *store.ClaimConflictError) {
	cwd := getCwd()
	blockers := append([]domain.ClaimRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].PathPrefix != blockers[j].PathPrefix {
			return blockers[i].PathPrefix < blockers[j].PathPrefix
		}
		return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ prefix=%s blocker=%s intent=%q expires_at=%s\n",
			relToCwd(b.PathPrefix, cwd), holderTag(string(b.OwnerUUID)), b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

// EmitClaimRelease renders the unclaim outcome and returns the suggested exit
// code: 0 for released / no-claim, 1 for not-owner. Render owns the rows and
// the code so the claim verb pair matches the lock pair's shape
// (EmitReleaseResults); the ✗ row names the actual live holder.
func EmitClaimRelease(w io.Writer, res store.ClaimReleaseResult) int {
	path := relToCwd(res.PathPrefix, getCwd())
	switch res.State {
	case store.ClaimStateReleased:
		fmt.Fprintf(w, "✓ unclaimed count=1\n✓ prefix=%s\n", path)
		return 0
	case store.ClaimStateNoClaim:
		fmt.Fprintf(w, "✓ unclaimed count=0\nℹ prefix=%s state=no-claim\n", path)
		return 0
	case store.ClaimStateNotOwner:
		// Name the holder like EmitClaimConflict does — matches the lock-side
		// not-owner row now that both route through holderTag (loto-a8t).
		fmt.Fprintf(w, "✓ unclaimed count=0\n✗ prefix=%s state=not-owner owner=%s\n", path, holderTag(res.Owner))
		return 1
	default:
		fmt.Fprintf(w, "✗ prefix=%s state=unknown\n", path)
		return 3
	}
}

// EmitClaimsReleased renders the claim half of a session-end `unlock --all`:
// the claim prefixes ReleaseBySession dropped. Prints a triage count first
// (design.md), then one ✓ row per prefix, cwd-relative. Empty input still emits
// the count=0 line so the claim leg is never silent — a hook reading this
// surface must be able to tell "no claims owned" from a crash. prefixes are
// pre-sorted by the store.
func EmitClaimsReleased(w io.Writer, prefixes []string) {
	fmt.Fprintf(w, "ℹ claims-released count=%d\n", len(prefixes))
	cwd := getCwd()
	for _, p := range prefixes {
		fmt.Fprintf(w, "✓ prefix=%s\n", relToCwd(p, cwd))
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
	fmt.Fprintf(w, "ℹ tags count=%d owner=%s\n", len(tags), holderTag(ownerUUID))
	for _, t := range tags {
		emitTagRow(w, t, "", cwd)
	}
}

// EmitTerritoryTagRows renders notes pinned to territory. `ℹ` and not `✓`/`✗`:
// a note is neither a pass nor a failure, it is a neutral fact about ground
// (.claude/rules/design.md). Empty input emits nothing — a repo with no notes
// keeps byte-identical output to one that never had the feature.
//
// header != "" prints the count line above the rows; the inline callers that
// already named their target pass "" and get rows only.
func EmitTerritoryTagRows(w io.Writer, notes []store.TerritoryTag, header, indent string) {
	if len(notes) == 0 {
		return
	}
	cwd := getCwd()
	if header != "" {
		fmt.Fprintf(w, "%sℹ %s count=%d\n", indent, header, len(notes))
	}
	for _, n := range notes {
		fmt.Fprintf(w, "%sℹ territory-tag id=%s from=%s prefix=%s expires_at=%s text=%q\n",
			indent, n.ID, holderTag(n.TaggerUUID), relToCwd(n.PathPrefix, cwd),
			time.Unix(0, n.ExpiresAt).UTC().Format(time.RFC3339), n.Text)
	}
}

// EmitExpiredTerritoryTags renders notes that lapsed with nobody having acked
// them — doctor's report, and the only place an expired note is ever shown
// (loto-z3y1 D2). `⚠` because it IS a finding: a note nobody read is the
// mail-lost failure this feature was meant to end, and reporting it is what
// keeps the failure from being silent. The text rides along so the row is
// actionable rather than a count.
func EmitExpiredTerritoryTags(w io.Writer, notes []store.TerritoryTag, now time.Time) {
	if len(notes) == 0 {
		return
	}
	cwd := getCwd()
	for _, n := range notes {
		age := now.Sub(time.Unix(0, n.ExpiresAt)).Round(time.Hour)
		fmt.Fprintf(w, "⚠ expired_territory_tag id=%s prefix=%s from=%s expired_ago=%s text=%q\n",
			n.ID, relToCwd(n.PathPrefix, cwd), holderTag(n.TaggerUUID), age, n.Text)
	}
}

// EmitTagRows renders just the per-tag lines (no count header). Use for inline
// blocks beneath a per-file status line where the surrounding context already
// names the target. Empty input emits nothing.
func EmitTagRows(w io.Writer, tags []store.Tag) {
	cwd := getCwd()
	for _, t := range tags {
		emitTagRow(w, t, "  ", cwd)
	}
}

// emitTagRow renders one tag line. cwd is passed in so callers hoist the
// os.Getwd() syscall out of their loops (the file convention documented on
// relToCwd; loto-kyib). The identity lookup that used to be hoisted alongside
// it is gone — a tagger is named by its owner id directly (loto-jnid).
func emitTagRow(w io.Writer, t store.Tag, indent, cwd string) {
	at := time.Unix(0, t.CreatedAt).UTC().Format(time.RFC3339)
	fmt.Fprintf(w, "ℹ %stag id=%s at=%s from=%s target=%s text=%q\n",
		indent, t.ID, at, holderTag(t.TaggerUUID), relToCwd(string(t.TargetCanonical), cwd), t.Text)
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
// exit code: 0 if no not-owner rows, 1 otherwise.
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
	exit := writeReleaseTriageLine(w, sorted)
	for _, r := range sorted {
		path := relToCwd(r.Target.Canonical, cwd)
		switch r.State {
		case store.StateUnlocked:
			fmt.Fprintf(w, "✓ target=%s\n", path)
		case store.StateReclaimedStale:
			fmt.Fprintf(w, "✓ target=%s state=reclaimed-stale owner=%s\n", path, r.Owner)
		case store.StateNoLock:
			fmt.Fprintf(w, "ℹ target=%s state=no-lock\n", path)
		case store.StateNotOwner:
			// Name the holder like EmitClaimConflict/EmitConflictWithTags do —
			// a bare UUID here forked the release-surface convention (loto-a8t).
			fmt.Fprintf(w, "✗ target=%s state=not-owner owner=%s\n", path, holderTag(r.Owner))
		}
	}
	return exit
}

// writeReleaseTriageLine emits the count-first triage line and returns the
// suggested exit code. Reclaimed-stale rows deleted rows just like an own
// release, but count under their own reclaimed= field (loto-ebkc): the caller
// released nothing it owned, and a reclaim is a success (exit 0).
func writeReleaseTriageLine(w io.Writer, sorted []store.ReleaseResult) int {
	successCount := 0
	reclaimed := 0
	exit := 0
	for _, r := range sorted {
		switch r.State {
		case store.StateUnlocked:
			successCount++
		case store.StateReclaimedStale:
			reclaimed++
		case store.StateNotOwner:
			exit = 1
		case store.StateNoLock:
			// no-op: nothing was owned at this target, not a release.
		}
	}
	fmt.Fprintf(w, "✓ unlocked count=%d", successCount)
	if reclaimed > 0 {
		fmt.Fprintf(w, " reclaimed=%d", reclaimed)
	}
	fmt.Fprintln(w)
	return exit
}

// EmitBreakResults renders per-target outcomes of `unlock --force` (BreakLocks).
// Clean breaks go to outW; problems — missing lock, authorize/break errors — go
// to errW. Returns the suggested exit code.
func EmitBreakResults(outW, errW io.Writer, results []store.BreakResult) int {
	cwd := getCwd()
	exit := 0
	for _, r := range results {
		path := relToCwd(r.Target.Canonical, cwd)
		switch {
		case r.Err == nil:
			fmt.Fprintf(outW, "✓ broken target=%s\n", path)
		case errors.Is(r.Err, store.ErrNoLockAtTarget):
			fmt.Fprintf(errW, "✗ no lock at target=%s\n", path)
			if exit < 1 {
				exit = 1
			}
		case errors.Is(r.Err, store.ErrHolderChanged):
			// A refused compare-and-swap is an advisory conflict (exit 1), not
			// an IO error (3): the store is fine, someone else holds the path.
			// Both hold sets print because the caller's next move depends on
			// which half moved — a new owner means back off, a bumped epoch on
			// the same owner means the holder cycled and a fresh read may
			// authorize the break after all.
			var hc *store.HolderChangedError
			if errors.As(r.Err, &hc) {
				fmt.Fprintf(errW, "✗ %s target=%s expected=%s actual=%s\n",
					store.ReasonHolderChanged, path,
					domain.FormatHoldRefs(hc.Expected), domain.FormatHoldRefs(hc.Actual))
			} else {
				fmt.Fprintf(errW, "✗ %s target=%s\n", store.ReasonHolderChanged, path)
			}
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
