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
  lease_state    TEXT NOT NULL DEFAULT '',
  expected_owner TEXT NOT NULL DEFAULT '',
  resolved_at    INTEGER,
  resolution     TEXT NOT NULL DEFAULT ''
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_violations_open_path
  ON violations(path_canonical) WHERE resolved_at IS NULL;
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
	LeaseState    string
	ExpectedOwner string
	ResolvedAt    *int64
	Resolution    string
}

// Resolved reports whether this row has been closed.
func (v Violation) Resolved() bool { return v.ResolvedAt != nil }

// ObservedViolation is the RecordViolations input: what the caller's scan saw
// plus the lease judgment only the store-holding caller can make.
type ObservedViolation struct {
	PathCanonical string
	Fingerprint   string
	LeaseState    string
	ExpectedOwner string
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
			Fingerprint: o.Fingerprint, LeaseState: o.LeaseState, ExpectedOwner: o.ExpectedOwner,
		}
		res, err := tx.ExecContext(ctx,
			`INSERT OR IGNORE INTO violations
			   (id, path_canonical, observed_at, fingerprint, lease_state, expected_owner, resolved_at, resolution)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, '')`,
			v.ID, v.PathCanonical, v.ObservedAt, v.Fingerprint, v.LeaseState, v.ExpectedOwner)
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
		`SELECT id, path_canonical, observed_at, fingerprint, lease_state, expected_owner, resolved_at, resolution
		   FROM violations WHERE resolved_at IS NULL ORDER BY path_canonical, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanViolations(rows)
}

// UnresolvedViolationPaths returns path -> open violation id, the exact shape
// admission's violation-intersect check consumes. A map rather than a slice
// because the check is a membership test per write-set path, and the id is
// what the rejection detail must name so the operator can act on it.
func (s *Store) UnresolvedViolationPaths(ctx context.Context) (map[string]string, error) {
	vs, err := s.UnresolvedViolations(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(vs))
	for _, v := range vs {
		out[v.PathCanonical] = v.ID
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
func (s *Store) ResolveViolationsForPaths(ctx context.Context, paths []string, resolution string) (int, error) {
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
			`UPDATE violations SET resolved_at = ?, resolution = ? WHERE path_canonical = ? AND resolved_at IS NULL`,
			nowNs, resolution, p)
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
			&v.LeaseState, &v.ExpectedOwner, &resolvedAt, &v.Resolution); err != nil {
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
// The same pass auto-closes any open row whose path is absent from obs. That
// is sound only because ScanWorktree is whole-tree: absence means the content
// agrees with integration again, i.e. the contamination was reverted. Feeding
// this a PARTIAL observation set would silently resolve every violation it
// did not look at — hence the whole-tree contract, stated here and enforced
// by having exactly one producer.
func (s *Store) ReconcileScan(ctx context.Context, obs []gate.Observation, ec domain.EvalContext) (ScanResult, error) {
	res := ScanResult{Observed: len(obs)}

	authorized, err := s.authorizedPaths(ctx, obs, ec)
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
		state, owner := s.lapsedLeaseWitness(ctx, o.Path, ec)
		candidates = append(candidates, ObservedViolation{
			PathCanonical: o.Path, Fingerprint: o.Fingerprint,
			LeaseState: state, ExpectedOwner: owner,
		})
	}

	// Auto-resolve BEFORE recording: a path cannot be both absent from obs
	// and newly recorded from it, so the two sets are disjoint and the order
	// only decides which failure leaves which half undone. Resolving first
	// means a mid-pass failure leaves stale-but-open rows rather than
	// prematurely-closed ones — the conservative direction for a check whose
	// whole job is to fail closed.
	open, err := s.UnresolvedViolations(ctx)
	if err != nil {
		return res, err
	}
	var reverted []string
	for _, v := range open {
		if _, still := seen[v.PathCanonical]; !still {
			reverted = append(reverted, v.PathCanonical)
		}
	}
	if res.Resolved, err = s.ResolveViolationsForPaths(ctx, reverted, ResolutionReverted); err != nil {
		return res, err
	}
	if res.Recorded, err = s.RecordViolations(ctx, candidates); err != nil {
		return res, err
	}
	return res, nil
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
