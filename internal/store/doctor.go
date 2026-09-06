package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
)

const (
	sqliteWALSuffix = "-wal"
	sqliteSHMSuffix = "-shm"
)

// vacuumFn is the VACUUM implementation called by DoctorRepair after a
// successful repair transaction. Tests can replace it to inject failures.
var vacuumFn = func(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `VACUUM`)
	return err
}

type DoctorReport struct {
	StaleLocks []domain.LockRecord
	// ExpiredClaims are the TTL-lapsed territory reservations that --repair
	// would sweep (gcClaimsTx), in (prefix, owner) order (loto-ebkc D3).
	ExpiredClaims []domain.ClaimRecord
	// ExpiredTerritoryTags are notes that lapsed with nobody having acked them
	// (loto-z3y1 D2). Reported here and nowhere else: a note that vanishes
	// unread is the mail-lost failure returning, and doctor is loto's
	// territory-health surface, so "a note expired unread on this ground" is a
	// fact about territory. Reported, not resurrected — no retry, no
	// re-delivery, no author notification (that would need an address).
	ExpiredTerritoryTags []TerritoryTag
	// StaleCandidateClaims are the candidate_claims rows --repair would sweep
	// (gcCandidateClaimsTx, loto-u2p7): abandoned by
	// domain.EvalContext.CandidateClaimIsAbandoned — provably dead, or
	// carrying no durable pid and no other liveness witness at all. In
	// (path, candidate_id) order (ListCandidateClaims' own order, preserved
	// by filtering rather than re-sorting).
	StaleCandidateClaims []domain.CandidateClaim
	SidecarFindings      []SidecarFinding
	IntegrityOK          bool
	IntegrityDetail      string
}

// SidecarCheck cross-checks held locks against the CC session sidecar to
// strengthen zombie detection. SidecarDir empty disables the check; RepoTop
// empty disables cwd-mismatch detection (no-sidecar still fires). The caller
// is the runtime layer which knows the on-disk paths.
type SidecarCheck struct {
	SidecarDir string
	RepoTop    string
}

func (s *Store) DoctorAudit(ctx context.Context, thisHost string, hostKnown bool, live domain.HolderLiveProbe, sc SidecarCheck) (*DoctorReport, error) {
	r := &DoctorReport{}
	locks, err := s.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	ec := domain.EvalContext{Now: time.Now(), Live: live, CaseFold: s.caseFold}
	for i := range locks {
		if ec.IsStale(locks[i]) {
			r.StaleLocks = append(r.StaleLocks, locks[i])
			continue
		}
		// PID <= 0 is the no-durable-pid sentinel (loto-j1bo): no CC session
		// sidecar is keyed by it, so the zombie cross-check has nothing to read
		// and would only emit a spurious no-cc-sidecar finding. Skip it.
		//
		// !hostKnown must gate ahead of the Host equality compare (loto-0yot,
		// mirrors liveProbe's loto-u7e guard): a host-unknown caller's
		// thisHost is untrustworthy, so "" == "" for another hostname-broken
		// machine's lock would otherwise pass and run the cross-check against
		// a pid from a foreign kernel. Skipping is safe — doctor only
		// reports; running against the wrong kernel produces a false finding.
		if hostKnown && locks[i].PID > 0 && locks[i].Host == thisHost && sc.SidecarDir != "" {
			if f, ok := checkSidecar(locks[i], sc); ok {
				r.SidecarFindings = append(r.SidecarFindings, f)
			}
		}
	}
	if err := s.collectExpiredClaims(ctx, r, ec.Now); err != nil {
		return nil, err
	}
	if err := s.collectStaleCandidateClaims(ctx, r, ec); err != nil {
		return nil, err
	}
	expiredNotes, err := s.ExpiredTerritoryTags(ctx, ec.Now.UnixNano())
	if err != nil {
		return nil, err
	}
	r.ExpiredTerritoryTags = expiredNotes
	if err := s.runIntegrityCheck(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// collectExpiredClaims fills r.ExpiredClaims with every TTL-lapsed claim in
// deterministic (prefix, owner) order — the audit half of the D3 sweep: what
// --repair's gcClaimsTx would delete (loto-ebkc).
func (s *Store) collectExpiredClaims(ctx context.Context, r *DoctorReport, now time.Time) error {
	claims, err := s.ListClaims(ctx)
	if err != nil {
		return err
	}
	for i := range claims {
		if claims[i].Expired(now) {
			r.ExpiredClaims = append(r.ExpiredClaims, claims[i])
		}
	}
	sort.Slice(r.ExpiredClaims, func(i, j int) bool {
		if r.ExpiredClaims[i].PathPrefix != r.ExpiredClaims[j].PathPrefix {
			return r.ExpiredClaims[i].PathPrefix < r.ExpiredClaims[j].PathPrefix
		}
		return r.ExpiredClaims[i].OwnerUUID < r.ExpiredClaims[j].OwnerUUID
	})
	return nil
}

// collectStaleCandidateClaims fills r.StaleCandidateClaims with every
// candidate_claims row domain.EvalContext.CandidateClaimIsAbandoned judges
// abandoned — the audit half of gcCandidateClaimsTx's sweep (loto-u2p7).
// ListCandidateClaims already returns (path_canonical, candidate_id) order;
// filtering preserves it rather than re-deriving a sort.
func (s *Store) collectStaleCandidateClaims(ctx context.Context, r *DoctorReport, ec domain.EvalContext) error {
	claims, err := s.ListCandidateClaims(ctx)
	if err != nil {
		return err
	}
	for i := range claims {
		if ec.CandidateClaimIsAbandoned(claims[i]) {
			r.StaleCandidateClaims = append(r.StaleCandidateClaims, claims[i])
		}
	}
	return nil
}

func (s *Store) runIntegrityCheck(ctx context.Context, r *DoctorReport) error {
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	r.IntegrityOK = len(lines) == 1 && lines[0] == "ok"
	if r.IntegrityOK {
		r.IntegrityDetail = "ok"
	} else {
		r.IntegrityDetail = strings.Join(lines, "; ")
	}
	return nil
}

// checkSidecar inspects ~/.claude/sessions/<pid>.json for a held lock and
// returns a finding when the sidecar is missing or its cwd doesn't match the
// repo the lock lives in. Indeterminate errors (e.g. permission denied) are
// silenced — they're not actionable zombie signals.
func checkSidecar(l domain.LockRecord, sc SidecarCheck) (SidecarFinding, bool) {
	got, err := readSidecar(sc.SidecarDir, l.PID)
	if err != nil {
		if sidecarMissing(err) {
			return SidecarFinding{
				PID:    l.PID,
				Target: l.Target.Canonical,
				Reason: SidecarReasonNoSidecar,
			}, true
		}
		return SidecarFinding{}, false
	}
	if sc.RepoTop == "" || got.CWD == "" {
		return SidecarFinding{}, false
	}
	if got.CWD != sc.RepoTop {
		return SidecarFinding{
			PID:    l.PID,
			Target: l.Target.Canonical,
			Reason: SidecarReasonCwdMismatch,
			Detail: got.CWD,
		}, true
	}
	return SidecarFinding{}, false
}

// DoctorRepair sweeps stale locks, expired claims, and orphaned tags. Host
// policy rides inside `live` (HolderLiveProbe takes the record), so unlike
// DoctorAudit — whose sidecar cross-check still needs a this-host string — it
// takes no host argument.
//
// The chmod-era migration resolves its repo-relative candidates against the
// store's own repo root (WithRepoTop). An unset root means "no repo here": the
// migration then touches only candidates that are already absolute, and never
// falls back to the process CWD (loto-gc82).
func (s *Store) DoctorRepair(ctx context.Context, agent domain.AgentUUID, live domain.HolderLiveProbe) error {
	byAgent := string(agent) // internal store helpers thread the owner as a plain string
	// Hold the op-flock across the tx AND the post-commit chmod restores
	// so concurrent AcquireLocks can't race the filesystem half of the
	// reclaim (loto-4qt).
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return err
	}
	defer flock.release()

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return err
	}
	now := time.Now()
	ec := domain.EvalContext{Now: now, Live: live, CaseFold: s.caseFold}
	if err := reclaimStaleLocks(ctx, tx, all, ec, byAgent, now); err != nil {
		return err
	}
	// Read the migration's candidate paths INSIDE the tx, before commit — the
	// events table is rotated by rotateEventsTx below, and a path whose last
	// event just aged out is exactly the one still sitting at 0444.
	candidates, err := chmodEraCandidates(ctx, tx, all, ec)
	if err != nil {
		return err
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := gcTagsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := gcClaimsTx(ctx, tx, now); err != nil {
		return err
	}
	if _, err := gcCandidateClaimsTx(ctx, tx, ec); err != nil {
		return err
	}
	if err := gcTerritoryTagsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The chmod-era migration runs under the still-held flock, then the flock is
	// released BEFORE the detached audit so its write tx can't extend the hold
	// for up to busy_timeout under contention (loto-3qev). VACUUM also runs after
	// release (loto-3bl0): it's post-commit, uses a fresh pool conn with SQLite's
	// own locking, and needs no op-flock.
	failEvents := restoreChmodEraFiles(s.repoTop, candidates, byAgent, now)
	flock.release()
	if len(failEvents) > 0 {
		_ = s.appendAuditDetached(failEvents)
	}
	if err := vacuumFn(ctx, s.db); err != nil {
		fmt.Fprintf(s.stderr, "loto: VACUUM after repair: %v (best-effort, repair succeeded)\n", err)
	}
	return nil
}

// reclaimStaleLocks deletes every stale lock row.
func reclaimStaleLocks(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, ec domain.EvalContext, byAgent string, now time.Time) error {
	for i := range all {
		if !ec.IsStale(all[i]) {
			continue
		}
		if err := reclaimStaleTx(ctx, tx, all[i], byAgent, now); err != nil {
			return err
		}
	}
	return nil
}

// chmodEraCandidates lists every path loto may have left read-only before it
// retired chmod enforcement (loto-zssw): the targets of current lock rows, plus
// every target named in the retained audit trail.
//
// ‡ This is a one-way migration with a natural end, not a permanent scan. Until
// 2026-08 acquire stripped write bits and release restored them, so an
// interrupted session could leave a file at 0444 with no row to explain it —
// observed in the wild, and the reason `doctor` grew an orphan mode. Nothing
// strips a bit any more, so once every stragger is repaired the pass finds
// only files that already carry owner-write and does nothing but stat them.
// The candidate set drains itself: the events table is retention-bounded, so a
// week after the upgrade it names no chmod-era path at all.
//
// Deliberately NOT a repo walk. loto has no repo to walk from here — the store
// is cross-repo — and a walk would be unbounded where this is bounded by rows
// loto already keeps.
//
// ‡ A LIVE lock's target is never a candidate (loto-2hjh). The migration's
// warrant is that a write-less path loto has a record of can only be a bit the
// old loto set and failed to restore. That inference holds for a path whose
// lock is gone or stale — nothing is editing it — and fails for one held right
// now: a live holder may have made the file read-only itself, and adding
// owner-write back under it is loto undoing protection on a path it is
// currently coordinating. Live rows are skipped here; `all` is the pre-reclaim
// snapshot, so a row this same repair is about to reclaim as stale still
// qualifies.
func chmodEraCandidates(ctx context.Context, tx *sql.Tx, all []domain.LockRecord, ec domain.EvalContext) ([]string, error) {
	live := make(map[string]bool, len(all))
	for i := range all {
		if !ec.IsStale(all[i]) {
			live[all[i].Target.Canonical] = true
		}
	}
	seen := make(map[string]bool, len(all))
	var out []string
	add := func(p string) {
		if p == "" || seen[p] || live[p] {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	for i := range all {
		add(all[i].Target.Canonical)
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT target_canonical FROM events WHERE target_canonical != ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		add(p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// restoreChmodEraFiles re-adds owner-write to every candidate that is still
// sitting write-less on disk, and returns the mode_restore_failed events for
// any that refused.
//
// Returning the events rather than writing them here is what keeps the audit
// off the flock critical section (loto-3qev); the caller emits them after
// releasing. A missing file, a symlink, or a path outside this machine is not
// an error — the candidate list is historical and most of it is gone.
//
// ‡ Candidates are repo-relative POSIX paths (`locks.target_canonical`, and the
// same form in `events`), so each one is resolved against repoTop before it is
// stat'd or chmod'd (loto-gc82). Resolving bare would deref against the process
// CWD, and `doctor --repair` runs from any subdirectory: a same-named write-less
// file under the caller's cwd would be chmod'd in place of the real target.
// Store-level fixtures carry absolute canonicals, so an already-absolute
// candidate is taken as-is.
func restoreChmodEraFiles(repoTop string, candidates []string, byAgent string, now time.Time) []domain.Event {
	var failEvents []domain.Event
	for _, p := range candidates {
		abs, ok := resolveChmodEraCandidate(repoTop, p)
		if !ok {
			continue
		}
		if !lacksOwnerWrite(abs) {
			continue
		}
		if rerr := restoreWrite(abs); rerr != nil {
			failEvents = append(failEvents, modeRestoreFailedEvent(p, byAgent, now, rerr))
		}
	}
	return failEvents
}

// resolveChmodEraCandidate turns a candidate path into the absolute path to
// stat. It reports false for a candidate that cannot be resolved without
// guessing — a repo-relative path with no repoTop — because the only available
// guess is the process CWD, which is the loto-gc82 bug.
func resolveChmodEraCandidate(repoTop, p string) (string, bool) {
	if filepath.IsAbs(p) {
		return p, true
	}
	if repoTop == "" {
		return "", false
	}
	return filepath.Join(repoTop, filepath.FromSlash(p)), true
}

// lacksOwnerWrite reports whether path is a regular file the owner cannot
// write. Anything else — missing, a directory, a symlink, unstatable — answers
// false, because the migration's job is to undo a bit loto set and it never set
// one on those.
func lacksOwnerWrite(path string) bool {
	fi, err := os.Lstat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	return fi.Mode().Perm()&0o200 == 0
}

// tagsRetentionAge: acked tags older than this are hard-deleted by doctor
// --repair. Matches events retention (7d) so the audit window is uniform.
const tagsRetentionAge = 7 * 24 * time.Hour

// gcTagsTx hard-deletes orphan tags (host lock gone) and old-acked tags.
// Runs inside DoctorRepair's tx alongside rotateEventsTx so the GC is atomic
// with the surrounding cleanup. Read-time filters already hide orphans, so
// this is purely a disk-reclamation pass — losing rows here is harmless.
func gcTagsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM tags
		WHERE NOT EXISTS (
		  SELECT 1 FROM locks l
		  WHERE l.target_canonical = tags.target_canonical
		    AND l.owner_uuid       = tags.lock_owner_uuid
		    AND l.created_at       = tags.lock_created_at
		)`); err != nil {
		return err
	}
	cutoff := now.Add(-tagsRetentionAge).UnixNano()
	_, err := tx.ExecContext(ctx, `
		DELETE FROM tags WHERE acked_at IS NOT NULL AND acked_at < ?`, cutoff)
	return err
}

// gcClaimsTx hard-deletes expired claims inside DoctorRepair's tx, beside
// gcTagsTx (loto-ebkc D3). `expires_at <= now` is the exact complement of the
// live-claim probe's `expires_at > now` (ReleaseClaim/partitionClaims), so a
// claim is either live or sweepable — never both, never neither. Without this
// sweep an expired claim accretes until a same-territory ClaimPrefix happens
// to overlap it. Purely disk reclamation: read paths already filter by Expired.
func gcClaimsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM claims WHERE expires_at <= ?`, now.UnixNano())
	return err
}

// gcCandidateClaimsTx deletes every row belonging to a candidate whose claim
// domain.EvalContext.CandidateClaimIsAbandoned judges abandoned (loto-u2p7),
// returning the number of rows removed (report.StaleCandidateClaims' count,
// when the caller wants dry-run parity). Runs inside DoctorRepair's tx beside
// gcClaimsTx.
//
// ‡ Deletes by CandidateID, not by individual (path, candidate_id) row — the
// same granularity ReleaseCandidateClaims already uses everywhere a
// candidate's claims are dropped for good (promoted/rejected/withdrawn). A
// candidate's rows all share the one minting process's PID/session/host
// (AcceptCandidate builds every row from one `submitter`), so the abandonment
// verdict is identical across a candidate's whole write-set — evaluating the
// first row found for each id is sufficient; nothing is gained by
// re-evaluating each path.
//
// ‡ The DECIDED scope (loto-u2p7 comment, 2026-09-05): this deletes the
// candidate_claims row(s) only. refs/loto/candidates/* and
// refs/loto/proposals/* are left untouched — the store, not refs, is the
// attribution home (loto-ovno.13), so the refs are inert once nothing blocks
// on them.
//
// ‡ ec carries the ambient triple rather than (live, now) positionally — the
// arg-order hazard domain.EvalContext exists to remove, and the only way this
// helper inherits the store's filesystem case class (loto-8soe).
func gcCandidateClaimsTx(ctx context.Context, tx *sql.Tx, ec domain.EvalContext) (int, error) {
	claims, err := listCandidateClaimsTx(ctx, tx)
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	var ids []string
	for i := range claims {
		id := claims[i].CandidateID
		if seen[id] {
			continue
		}
		seen[id] = true
		if ec.CandidateClaimIsAbandoned(claims[i]) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids) // deterministic delete order, matching this file's other sweeps
	removed := 0
	for _, id := range ids {
		res, err := tx.ExecContext(ctx, `DELETE FROM candidate_claims WHERE candidate_id = ?`, id)
		if err != nil {
			return removed, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return removed, err
		}
		removed += int(n)
	}
	return removed, nil
}

// errOrphanNoRepoTop reports a candidate/lock-row path-form mismatch the store
// cannot reconcile. Loud on purpose: the alternative is a filter that silently
// answers "nothing is owned", which is exactly the loto-qoic bug.
var errOrphanNoRepoTop = errors.New("orphan scan: cannot reconcile candidate paths with repo-relative lock rows")

// validateOrphanRoot refuses the two bases that would silently reproduce
// loto-qoic. An empty repoTop cannot join a repo-relative row up to an absolute
// candidate; a relative repoTop joins to something that can never equal one.
// A repoTop that is absolute but WRONG stays unguarded — the store cannot
// validate a base it does not own (see loto-6e02).
func (s *Store) validateOrphanRoot(paths []string) error {
	repoTop := s.repoTop
	if repoTop == "" {
		for _, p := range paths {
			if filepath.IsAbs(p) {
				return fmt.Errorf("%w: absolute candidate %q with empty repoTop", errOrphanNoRepoTop, p)
			}
		}
		return nil
	}
	if !filepath.IsAbs(repoTop) {
		return fmt.Errorf("%w: repoTop %q is not absolute", errOrphanNoRepoTop, repoTop)
	}
	return nil
}

// lockedPathSet returns the absolute paths that currently hold a LIVE lock row,
// keyed to match doctor's candidates.
//
// ‡ Path-form contract (loto-qoic). locks.target_canonical is REPO-RELATIVE:
// domain.Canonicalize rejects a leading '/', and every production writer reaches
// the column through internal/cli/paths.go's normalizeRepoPath. Doctor's
// candidates are ABSOLUTE, and must stay so — ScanOrphanModes os.Stat's them and
// RestoreOrphanMode chmods them, both relative to process CWD, while doctor runs
// from any subdirectory. The two key spaces are disjoint, so something must join
// them; this is that place, because it is the function that performs the
// comparison. Rows are joined UP to absolute rather than candidates relativized
// down, only because rows are few and candidates are thousands.
//
// The raw stored row is deliberately NOT also used as a key. An absolute
// target_canonical cannot be written by any current path, so that arm would only
// ever match hand-built test fixtures — the same fiction that let this bug live
// in a green suite for months.
//
// ‡ Liveness uses domain.EvalContext.IsStale — the SAME predicate reclaimStaleLocks
// applies — and not a bare `expires_at > now`. The difference is the whole point of
// orphan mode: a holder that is SIGKILLed before its TTL lapses leaves a read-only
// file behind with a row that has not yet expired (loto-j863). Filtering on expiry
// alone would suppress exactly that file — a false negative in the crash case
// orphan recovery exists to surface. One predicate, no drift (staleness.go).
// A nil probe degrades safely to TTL-only.
func (s *Store) lockedPathSet(ctx context.Context, ec domain.EvalContext) (map[string]bool, error) {
	recs, err := s.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for i := range recs {
		if ec.IsStale(recs[i]) {
			continue
		}
		c := recs[i].Target.Canonical
		if s.repoTop == "" {
			owned[c] = true
			continue
		}
		owned[filepath.Join(s.repoTop, filepath.FromSlash(c))] = true
	}
	return owned, nil
}

// ScanOrphanModes returns paths that are read-only on disk but have no
// matching live lock row, where "live" is domain.EvalContext.IsStale inverted:
// neither past its TTL nor held by a provably dead owner. Caller supplies the candidate paths (typically all
// regular files under the repo, or a curated subset).
//
// ‡ Candidates must be ABSOLUTE and rooted at repoTop; lock rows are
// repo-relative, and repoTop is what joins the two. This narrows the older
// "caller supplies the candidates" contract — see lockedPathSet for why the
// store owns the comparison (loto-qoic). Only the empty-base and relative-base
// mistakes are machine-checked (validateOrphanRoot); an absolute-but-wrong
// repoTop cannot be caught here.
//
// ‡ Sibling of restoreChmodEraFiles, and the split between them is about
// evidence. This one walks the REPO, so a read-only file it finds may simply
// be one the user meant to protect — hence the explicit --restore-orphan-mode
// gate. restoreChmodEraFiles walks paths loto's own lock rows and audit trail
// name, MINUS the ones a live lock holds: on a path nothing is editing, a
// missing write bit is chmod-era residue and nothing else, so it runs
// unprompted. On a live-locked path it is not — the holder may have made the
// file read-only itself — which is why live rows are excluded (loto-2hjh).
func (s *Store) ScanOrphanModes(ctx context.Context, live domain.HolderLiveProbe, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	if err := s.validateOrphanRoot(paths); err != nil {
		return nil, err
	}
	owned, err := s.lockedPathSet(ctx, domain.EvalContext{Now: time.Now(), Live: live, CaseFold: s.caseFold})
	if err != nil {
		return nil, err
	}

	var orphans []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.Mode().IsRegular() {
			continue
		}
		if st.Mode().Perm()&0o222 != 0 {
			continue
		}
		if owned[p] {
			continue
		}
		orphans = append(orphans, p)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// RestoreOrphanMode chmods the given paths back to owner-writable. Caller
// gates this behind explicit user intent (--restore-orphan-mode). Returns
// the paths it successfully chmod'd and a parallel slice of per-path failures
// so callers can surface why a file was skipped.
//
// ‡ Same path-form contract as ScanOrphanModes: absolute candidates, rooted at
// repoTop, reconciled against repo-relative rows (loto-qoic). The re-check below
// is only meaningful because that reconciliation happens here, inside the
// op-flock — a caller-side pre-filter cannot cover the same window.
//
// Holds the project op-flock across the full restore so a concurrent
// AcquireLocks/Release/Break can't mutate the lock set mid-restore and
// leave the filesystem and DB views torn (loto-98v, gh#124). Same posture
// as DoctorRepair, which already serializes its DB+chmod work under op-flock.
func (s *Store) RestoreOrphanMode(ctx context.Context, live domain.HolderLiveProbe, paths []string) (restored []string, failures []OrphanRestoreFailure, err error) {
	if err := s.validateOrphanRoot(paths); err != nil {
		return nil, nil, err
	}
	flock, err := acquireOpFlock(ctx, s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, nil, err
	}
	defer flock.release()

	// Re-validate ownership under the held op-flock: build the set of currently
	// locked paths so we can skip any that became locked since the caller's scan
	// (TOCTOU fix for loto-h85e). The flock ensures the lock table is stable for
	// the duration of this query + chmod loop, and the query MUST stay inside it.
	nowOwned, err := s.lockedPathSet(ctx, domain.EvalContext{Now: time.Now(), Live: live, CaseFold: s.caseFold})
	if err != nil {
		return nil, nil, fmt.Errorf("RestoreOrphanMode: re-query locks: %w", err)
	}

	for _, p := range paths {
		if nowOwned[p] {
			// Path acquired a lock after the caller's scan — skip to avoid
			// defeating the freshly-held lock's write-strip.
			continue
		}
		if err := restoreWrite(p); err != nil {
			failures = append(failures, OrphanRestoreFailure{Path: p, Err: err})
			continue
		}
		restored = append(restored, p)
	}
	return restored, failures, nil
}

// OrphanRestoreFailure pairs an orphan-mode path with the error encountered
// while trying to chmod it back to writable.
type OrphanRestoreFailure struct {
	Path string
	Err  error
}

// moveCorruptAside relocates a corrupt DB and its -wal/-shm siblings into
// a single sibling directory <dbPath>.corrupt.<RFC3339Z>/. The move is
// atomic: files are first assembled in a staging directory, which is then
// renamed into place with one os.Rename. This eliminates the race in the
// previous three-rename approach, where a concurrent opener could see a
// fresh main DB paired with a stale sidecar (gh#48).
func moveCorruptAside(dbPath string, when time.Time) (string, error) {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	stamp := when.UTC().Format("2006-01-02T15-04-05Z")
	finalDir := fmt.Sprintf("%s.corrupt.%s", dbPath, stamp)

	staging, err := os.MkdirTemp(dir, base+".corrupt-staging-")
	if err != nil {
		return "", fmt.Errorf("make staging dir: %w", err)
	}
	// holdsCorruptBytes flips true after the first os.Rename(dbPath, …) — at
	// that point staging contains the only copy of the user's corrupt DB.
	// Wiping it on later failure (the original RemoveAll defer) is data loss
	// the user can't recover from (audit loto-2y6). Instead, requarantine the
	// staging dir under a *.corrupt.failed.<stamp>/ name so the forensic bytes
	// survive even when the final commit-rename can't proceed.
	holdsCorruptBytes := false
	committed := false
	defer func() {
		if committed {
			return
		}
		if !holdsCorruptBytes {
			_ = os.RemoveAll(staging)
			return
		}
		failed := fmt.Sprintf("%s.corrupt.failed.%s", dbPath, stamp)
		if err := os.Rename(staging, failed); err != nil {
			// Last resort: leave staging in place. The dir name still encodes
			// "corrupt-staging-" so it's discoverable even unrenamed.
			fmt.Fprintf(os.Stderr, "loto: corrupt DB bytes preserved at %s (rename to %s failed: %v)\n", staging, failed, err)
		} else {
			// Best-effort: flush the parent so the requarantine entry is durable.
			// A defer can only log, never return — match the recovery-path tone.
			_ = syncDir(dir)
			fmt.Fprintf(os.Stderr, "loto: corrupt DB bytes preserved at %s after commit-rename failure\n", failed)
		}
	}()

	moved, err := fillCorruptStaging(dbPath, staging, base)
	holdsCorruptBytes = moved
	if err != nil {
		return "", err
	}

	if err := os.Rename(staging, finalDir); err != nil {
		return "", fmt.Errorf("commit corrupt dir: %w", err)
	}
	// committed first: the deferred cleanup must not chase a staging path that
	// no longer exists once the commit-rename has consumed it.
	committed = true
	// Flush the parent so the new <finalDir> entry survives power loss.
	if err := syncDir(dir); err != nil {
		return "", fmt.Errorf("sync corrupt dir parent: %w", err)
	}
	return finalDir, nil
}

// fillCorruptStaging moves the corrupt main DB and any existing -wal/-shm
// siblings into staging, then fsyncs staging so the moved entries are durable
// before it is published (loto-4n65). The bool return is true once the main DB
// has been renamed in — from that point staging holds the only copy of the
// user's bytes, so the caller must preserve it on any later failure (see the
// moveCorruptAside defer). It stays true even when a sibling rename or the
// staging fsync fails.
func fillCorruptStaging(dbPath, staging, base string) (bool, error) {
	if err := os.Rename(dbPath, filepath.Join(staging, base)); err != nil {
		return false, fmt.Errorf("rename main: %w", err)
	}
	for _, sfx := range []string{sqliteWALSuffix, sqliteSHMSuffix} {
		src := dbPath + sfx
		// Rename directly and tolerate a missing sibling rather than Stat-then-
		// Rename: the stat+rename pair is a TOCTOU race, and a not-exist error
		// here just means there was no WAL/SHM to quarantine.
		if err := os.Rename(src, filepath.Join(staging, base+sfx)); err != nil && !os.IsNotExist(err) {
			return true, fmt.Errorf("rename %s: %w", sfx, err)
		}
	}
	if err := syncDir(staging); err != nil {
		return true, fmt.Errorf("sync staging: %w", err)
	}
	return true, nil
}

// syncDir flushes a directory's metadata to stable storage so that a rename
// performed inside it survives power loss. Call after the file/dir itself has
// been fsync'd. (Duplicated from internal/identity & internal/cli rather than
// shared via a helper package: store may depend only on internal/domain — see
// .go-arch-lint.yml. The helper is small enough to fall under jscpd limits.)
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}
