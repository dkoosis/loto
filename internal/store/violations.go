package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
)

// violationsDDL is the violations table shape, duplicated from schema.sql per
// the claimsDDL / territoryTagsDDL precedent: a pending ensureFn must be able
// to apply itself. No user_version bump — bumping trips the move-aside path
// and would destroy live locks (loto-kwlp).
const violationsDDL = `
CREATE TABLE IF NOT EXISTS violations (
  id             TEXT PRIMARY KEY,
  path_canonical TEXT NOT NULL,
  observed_at    INTEGER NOT NULL,
  fingerprint    TEXT NOT NULL DEFAULT '',
  baseline       TEXT NOT NULL DEFAULT '',
  lease_state    TEXT NOT NULL DEFAULT '',
  expected_owner TEXT NOT NULL DEFAULT '',
  resolved_at    INTEGER,
  resolution     TEXT NOT NULL DEFAULT '',
  worktree       TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_violations_open_path_wt
  ON violations(path_canonical, worktree) WHERE resolved_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_violations_open ON violations(resolved_at, path_canonical);`

// Lease states a violation can be observed under. Both mean "nothing
// authorized this write"; they differ in what the record can say about who
// was supposed to be there, which is forensic, never attribution.
const (
	// LeaseStateUnleased: no lock, beacon, or candidate claim has ever
	// covered this path in a way the scan could see.
	LeaseStateUnleased = "unleased"
	// LeaseStateExpired: a lock row exists for the path but is stale — the
	// authorization lapsed before the content was observed changed.
	LeaseStateExpired = "expired-lease"
)

// WorktreeLegacy marks an open violation recorded before rows carried a
// checkout — its origin is genuinely unknown, and guessing is unsafe in one
// direction only.
//
// ‡ Assigning legacy rows to the primary worktree (”) would let a linked
// checkout upgrade straight past a sticky violation it had itself recorded:
// its scoped intersect stops seeing the row, and if it has since taken a
// lease on that path its own scan records nothing new (a leaseholder's edit
// is not a violation) — so the contaminated content submits clean. That is
// precisely the laundering the record exists to stop (Codex #283 P1).
//
// A legacy row therefore blocks EVERY checkout's admission, and any
// checkout's clean pass may resolve it — which is exactly how these rows
// behaved before the column existed, so the migration makes admission
// strictly safer and resolution no harder.
const WorktreeLegacy = "?"

// Resolutions a violation row can close with.
const (
	// ResolutionReverted: a later scan found the path's content back in
	// agreement with integration. Machine-observed, needs no human.
	ResolutionReverted = "reverted"
	// ResolutionAcked: a human or agent judged the change legitimate and said
	// so on the record. Never silent — the whole point of stickiness is that
	// contamination cannot be cleared by simply working past it.
	ResolutionAcked = "acked"
)

// Violation is one recorded unauthorized mutation: content in the working
// tree that differs from refs/loto/integration on a path nothing authorized.
//
// ‡ Sticky by design. The row survives a lease acquired AFTER it was
// recorded, and that is the correctness property the whole record exists for:
// without it, `loto lock p && loto submit p` on a contaminated path launders
// a rogue edit into integration under a perfectly valid lease. Admission's
// violation-intersect check reads exactly this table.
//
// ‡ No culprit field, ever. The sensor sees content, not writers
// (git-gate.md: "no process attribution claimed"). ExpectedOwner names who
// HELD the lapsed lease, not who wrote — a distinction the column name keeps
// visible so a future reader cannot mistake it for attribution.
type Violation struct {
	ID            string
	PathCanonical string
	ObservedAt    int64
	Fingerprint   string
	// Baseline is the refs/loto/integration commit the observation was a
	// delta FROM. An acknowledgement is only meaningful against it: when
	// integration moves, the same (path, fingerprint) can mean something
	// entirely different (Codex #276 round 2).
	Baseline      string
	LeaseState    string
	ExpectedOwner string
	// Worktree is the checkout the observation was taken in — "" for the
	// primary one. Two worktrees of one repo share this store, so a row
	// without it cannot say WHOSE tree is dirty, and a clean pass from one
	// checkout would resolve the other's rows (loto-nper).
	Worktree   string
	ResolvedAt *int64
	Resolution string
}

// Resolved reports whether this row has been closed.
func (v Violation) Resolved() bool { return v.ResolvedAt != nil }

// ObservedViolation is the RecordViolations input: what the caller's scan saw
// plus the lease judgment only the store-holding caller can make.
type ObservedViolation struct {
	PathCanonical string
	Fingerprint   string
	Baseline      string
	LeaseState    string
	ExpectedOwner string
	Worktree      string
}

func newViolationID() string { return newID("v-") }

// ErrUnknownViolation reports a resolve against an id no row carries.
var ErrUnknownViolation = errors.New("loto: no such violation")

// RecordViolations inserts one open row per path that does not already have
// one, and returns the rows it created. Re-observing a path with an open row
// is a NO-OP, deliberately: the record is of the FIRST time the contamination
// was seen, and re-stamping observed_at on every scan would erase how long a
// path has been dirty — the one thing the row is uniquely able to say.
//
// Idempotence is enforced by the partial unique index on open rows rather
// than by a read-then-write, so two loto processes scanning the same dirty
// tree concurrently cannot both insert. The insert is INSERT OR IGNORE and
// the caller learns what actually landed from the return value.
func (s *Store) RecordViolations(ctx context.Context, obs []ObservedViolation) ([]Violation, error) {
	if len(obs) == 0 {
		return nil, nil
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	nowNs := time.Now().UnixNano()
	var recorded []Violation
	for _, o := range obs {
		v := Violation{
			ID: newViolationID(), PathCanonical: o.PathCanonical, ObservedAt: nowNs,
			Fingerprint: o.Fingerprint, Baseline: o.Baseline,
			LeaseState: o.LeaseState, ExpectedOwner: o.ExpectedOwner,
			Worktree: o.Worktree,
		}
		// The NOT EXISTS clause re-checks the acknowledgement INSIDE this
		// transaction. ReconcileScan reads acks before it gets here, and a
		// `loto violations resolve` committing in that window would leave the
		// scan inserting a fresh open row for content the operator was just
		// told was cleared — a resolve that visibly un-resolves itself
		// (Codex #276 round 2, loto-njaj). The stale snapshot can only ever
		// be too permissive, so re-asking at write time closes the window
		// without needing the read inside the same tx.
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO violations
			   (id, path_canonical, observed_at, fingerprint, baseline, lease_state, expected_owner, worktree, resolved_at, resolution)
			 SELECT ?, ?, ?, ?, ?, ?, ?, ?, NULL, ''
			  WHERE NOT EXISTS (
			        SELECT 1 FROM violations
			         WHERE path_canonical = ? AND fingerprint = ? AND baseline = ? AND worktree = ?
			           AND resolved_at IS NOT NULL AND resolution != ?)`,
			v.ID, v.PathCanonical, v.ObservedAt, v.Fingerprint, v.Baseline, v.LeaseState, v.ExpectedOwner, v.Worktree,
			v.PathCanonical, v.Fingerprint, v.Baseline, v.Worktree, ResolutionReverted)
		if err != nil {
			return nil, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			recorded = append(recorded, v)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	sort.Slice(recorded, func(i, j int) bool { return recorded[i].PathCanonical < recorded[j].PathCanonical })
	return recorded, nil
}

// UnresolvedViolations returns every open row, sorted by path then id — the
// deterministic order every surface that renders them relies on
// (.claude/rules/design.md: same input, byte-identical output).
func (s *Store) UnresolvedViolations(ctx context.Context) ([]Violation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path_canonical, observed_at, fingerprint, baseline, lease_state, expected_owner, worktree, resolved_at, resolution
		   FROM violations WHERE resolved_at IS NULL ORDER BY path_canonical, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanViolations(rows)
}

// UnresolvedViolationsIn returns the open rows recorded from ONE checkout.
//
// ‡ The scoped read, not the repo-wide one, is what auto-resolution and
// admission consume. Two worktrees of one repo share this store, and a
// whole-tree pass speaks only for the tree it walked: reconciling against
// every row would let a clean checkout close a dirty one's findings, and
// intersecting against every row would refuse a clean checkout's submit for
// contamination sitting in a tree it cannot even see (loto-nper). The
// operator-facing report keeps using UnresolvedViolations — a human asking
// "what is dirty here" means the repo.
func (s *Store) UnresolvedViolationsIn(ctx context.Context, worktree string) ([]Violation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path_canonical, observed_at, fingerprint, baseline, lease_state, expected_owner, worktree, resolved_at, resolution
		   FROM violations
		  WHERE resolved_at IS NULL AND (worktree = ? OR worktree = ?)
		  ORDER BY path_canonical, id`, worktree, WorktreeLegacy)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanViolations(rows)
}

// ViolationByID returns one row, open or closed, by id. The closed half is
// the point: a resolved row carries WHY it was cleared, and a record no
// surface can read back is not a record.
func (s *Store) ViolationByID(ctx context.Context, id string) (Violation, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path_canonical, observed_at, fingerprint, baseline, lease_state, expected_owner, worktree, resolved_at, resolution
		   FROM violations WHERE id = ?`, id)
	if err != nil {
		return Violation{}, err
	}
	defer rows.Close()
	vs, err := scanViolations(rows)
	if err != nil {
		return Violation{}, err
	}
	if len(vs) == 0 {
		return Violation{}, ErrUnknownViolation
	}
	return vs[0], nil
}

// UnresolvedViolationPaths returns path -> open violation id for one
// checkout, the exact shape admission's violation-intersect check consumes. A
// map rather than a slice because the check is a membership test per
// write-set path, and the id is what the rejection detail must name so the
// operator can act on it.
//
// Scoped to worktree for the same reason the scan is: a candidate proposes
// content from ONE tree, so contamination in another one neither taints it
// nor may block it (loto-nper).
func (s *Store) UnresolvedViolationPaths(ctx context.Context, worktree string) (map[string]string, error) {
	vs, err := s.UnresolvedViolationsIn(ctx, worktree)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vs))
	for i := range vs {
		out[vs[i].PathCanonical] = vs[i].ID
	}
	return out, nil
}

// ResolveViolation closes one open row by id. An id that names no row, or one
// already closed, returns ErrUnknownViolation rather than succeeding silently
// — a resolve the operator believes happened and did not is worse than a
// visible refusal.
func (s *Store) ResolveViolation(ctx context.Context, id, resolution string) error {
	if resolution == "" {
		resolution = ResolutionAcked
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	res, err := tx.ExecContext(ctx,
		`UPDATE violations SET resolved_at = ?, resolution = ? WHERE id = ? AND resolved_at IS NULL`,
		time.Now().UnixNano(), resolution, id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrUnknownViolation
	}
	return tx.Commit()
}

// ResolveViolationsForPaths closes every open row on the given paths and
// returns how many it closed — the auto-resolution door. Its one production
// caller is the scan: a path whose content is back in agreement with
// integration is no longer contaminated, and requiring a human to say so
// would make the reverting fix look like it had not worked.
//
// Unlike ResolveViolation, a path with no open row is not an error: the
// caller is reconciling a set, not acting on a specific record.
func (s *Store) ResolveViolationsForPaths(ctx context.Context, worktree string, paths []string, resolution string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	if resolution == "" {
		resolution = ResolutionReverted
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return 0, err
	}
	defer cleanup()

	nowNs := time.Now().UnixNano()
	total := 0
	for _, p := range paths {
		res, err := tx.ExecContext(ctx,
			`UPDATE violations SET resolved_at = ?, resolution = ?
			  WHERE path_canonical = ? AND (worktree = ? OR worktree = ?) AND resolved_at IS NULL`,
			nowNs, resolution, p, worktree, WorktreeLegacy)
		if err != nil {
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			total += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return total, nil
}

func scanViolations(rows *sql.Rows) ([]Violation, error) {
	var out []Violation
	for rows.Next() {
		var v Violation
		var resolvedAt sql.NullInt64
		if err := rows.Scan(&v.ID, &v.PathCanonical, &v.ObservedAt, &v.Fingerprint,
			&v.Baseline, &v.LeaseState, &v.ExpectedOwner, &v.Worktree, &resolvedAt, &v.Resolution); err != nil {
			return nil, err
		}
		if resolvedAt.Valid {
			at := resolvedAt.Int64
			v.ResolvedAt = &at
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ScanResult is what one ReconcileScan pass did: how many observations it was
// handed, which open rows it created, and how many it auto-closed.
type ScanResult struct {
	Observed int
	Recorded []Violation
	Resolved int
}

// ReconcileScan turns one whole-tree sensor pass into violation rows.
//
// An observation becomes a violation exactly when NOTHING authorizes the
// path: no live lock (of any owner, any mode — a shared beacon authorizes its
// holder's write just as an exclusive lease does) and no live candidate claim.
// A leaseholder's own edit is therefore never a violation, which is the
// stated residual hole: a rogue edit INSIDE a held file is indistinguishable
// from the holder's work (git-gate.md Outcome 1).
//
// The same pass auto-closes any open row whose path is absent from obs, and
// only among the rows recorded from scan.Worktree. That is sound only because
// ScanWorktree is whole-tree: absence means the content agrees with
// integration again, i.e. the contamination was reverted. Feeding this a
// PARTIAL observation set would silently resolve every violation it did not
// look at — hence the whole-tree contract, stated here and enforced by having
// exactly one producer.
//
// ‡ "Whole-tree" is whole ONE tree. Worktrees of a repo share this store, so
// a pass is scoped to the checkout it walked: without that, running a scan
// from a dispatch worktree closes the main checkout's findings while the
// contaminated content is still sitting on its disk (loto-nper).
func (s *Store) ReconcileScan(ctx context.Context, scan gate.Scan, ec domain.EvalContext) (ScanResult, error) {
	obs := scan.Observations
	res := ScanResult{Observed: len(obs)}

	authorized, err := s.authorizedPaths(ctx, obs, ec)
	if err != nil {
		return res, err
	}
	paths := make([]string, 0, len(obs))
	for _, o := range obs {
		paths = append(paths, o.Path)
	}
	acked, err := s.ackedFingerprints(ctx, paths, scan.Baseline, scan.Worktree)
	if err != nil {
		return res, err
	}
	seen := make(map[string]struct{}, len(obs))
	var candidates []ObservedViolation
	for _, o := range obs {
		seen[o.Path] = struct{}{}
		if _, ok := authorized[o.Path]; ok {
			continue
		}
		// A human already looked at exactly this content on this path and
		// said it is legitimate and staying (`loto violations resolve`,
		// any resolution other than the machine ResolutionReverted). Content
		// unchanged since that ack must not be re-flagged on every later
		// scan — that would make "use resolve for a change that is
		// legitimate and staying" (the CLI's own guidance) false in
		// practice (Codex #276 P2). A DIFFERENT fingerprint on the same
		// path is a new mutation and still gets flagged.
		if _, ok := acked[o.Path+"\x00"+o.Fingerprint]; ok {
			continue
		}
		state, owner := s.lapsedLeaseWitness(ctx, o.Path, ec)
		candidates = append(candidates, ObservedViolation{
			PathCanonical: o.Path, Fingerprint: o.Fingerprint, Baseline: scan.Baseline,
			LeaseState: state, ExpectedOwner: owner, Worktree: scan.Worktree,
		})
	}

	// Auto-resolve BEFORE recording: a path cannot be both absent from obs
	// and newly recorded from it, so the two sets are disjoint and the order
	// only decides which failure leaves which half undone. Resolving first
	// means a mid-pass failure leaves stale-but-open rows rather than
	// prematurely-closed ones — the conservative direction for a check whose
	// whole job is to fail closed.
	open, err := s.UnresolvedViolationsIn(ctx, scan.Worktree)
	if err != nil {
		return res, err
	}
	var reverted []string
	for i := range open {
		if _, still := seen[open[i].PathCanonical]; !still {
			reverted = append(reverted, open[i].PathCanonical)
		}
	}
	if res.Resolved, err = s.ResolveViolationsForPaths(ctx, scan.Worktree, reverted, ResolutionReverted); err != nil {
		return res, err
	}
	if res.Recorded, err = s.RecordViolations(ctx, candidates); err != nil {
		return res, err
	}
	return res, nil
}

// ackedFingerprints returns, keyed "path\x00fingerprint", every (path,
// content) pair a human has explicitly resolved with something other than
// the machine ResolutionReverted — i.e. every fingerprint someone has looked
// at and said is legitimate. Membership is the only operation the caller
// needs, hence the flat key rather than a nested map.
func (s *Store) ackedFingerprints(ctx context.Context, paths []string, baseline, worktree string) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	if len(paths) == 0 {
		return out, nil
	}
	placeholders, args := inClauseStrings(paths)
	args = append(args, ResolutionReverted, baseline, worktree)
	q := `SELECT path_canonical, fingerprint FROM violations` + //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		` WHERE path_canonical IN (` + placeholders + `) AND resolved_at IS NOT NULL` +
		` AND resolution != ? AND baseline = ? AND worktree = ?`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var path, fp string
		if err := rows.Scan(&path, &fp); err != nil {
			return nil, err
		}
		out[path+"\x00"+fp] = struct{}{}
	}
	return out, rows.Err()
}

// authorizedPaths is the set of observed paths something live covers — a live
// lock or a live candidate claim. Both reads are whole-table-cheap at loto's
// scale and are taken once per pass rather than once per path.
func (s *Store) authorizedPaths(ctx context.Context, obs []gate.Observation, ec domain.EvalContext) (map[string]struct{}, error) {
	out := make(map[string]struct{}, len(obs))
	locks, err := s.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	live := make(map[string]struct{}, len(locks))
	for i := range locks {
		if !ec.IsStale(locks[i]) {
			live[locks[i].Target.Canonical] = struct{}{}
		}
	}
	paths := make([]string, 0, len(obs))
	for _, o := range obs {
		paths = append(paths, o.Path)
		if _, ok := live[o.Path]; ok {
			out[o.Path] = struct{}{}
		}
	}
	claims, err := s.CandidateClaimsForPaths(ctx, paths)
	if err != nil {
		return nil, err
	}
	for _, c := range claims {
		if !ec.CandidateClaimIsDead(c) {
			out[c.PathCanonical] = struct{}{}
		}
	}
	return out, nil
}

// lapsedLeaseWitness reports what the record can honestly say about the
// authorization that ISN'T there: a stale lock row means the path was leased
// and the lease lapsed, and names the owner it lapsed from. Nothing here is
// attribution — the lapsed holder is not accused of the write, and the field
// it lands in is named expected_owner for exactly that reason.
//
// A read failure degrades to "unleased" rather than propagating: the caller
// is recording a violation it has already decided on, and losing the forensic
// annotation must not lose the violation.
func (s *Store) lapsedLeaseWitness(ctx context.Context, path string, ec domain.EvalContext) (state, owner string) {
	locks, err := s.LocksAt(ctx, domain.Target{Canonical: path})
	if err != nil {
		return LeaseStateUnleased, ""
	}
	for i := range locks {
		if ec.IsStale(locks[i]) {
			return LeaseStateExpired, string(locks[i].OwnerUUID)
		}
	}
	return LeaseStateUnleased, ""
}
