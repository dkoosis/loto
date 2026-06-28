package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"loto/internal/domain"
)

// Tag is the persisted form of a `loto tag` annotation. Lifetime is parasitic
// on the host lock identified by (TargetCanonical, LockOwnerUUID, LockCreatedAt);
// when the host lock disappears the tag becomes orphaned and is filtered from
// alive lists at read time, then hard-deleted by doctor --repair.
type Tag struct {
	ID                              string
	TargetCanonical                 domain.Canonical
	LockOwnerUUID, TaggerUUID, Text string
	LockCreatedAt, CreatedAt        int64
	AckedAt                         *int64
}

// NewTag is the InsertTag input. Caller resolves the host-lock triple
// (target, owner, created_at) from the live `locks` row before calling.
type NewTag struct {
	TargetCanonical                 domain.Canonical
	LockOwnerUUID, TaggerUUID, Text string
	LockCreatedAt                   int64
}

const tagCap = 5

// tagTextMaxBytes bounds tag.text at the write site so a misbehaving caller
// can't balloon the DB with megabyte annotations (gh#129). 4 KiB is generous
// for human-meaningful "why?" notes (a screenful of prose) while keeping the
// per-row footprint bounded. Mirrored as a CHECK(length(text) <= 4096) in
// schema.sql so direct SQL writes can't bypass the Go-side guard.
const tagTextMaxBytes = 4096

var (
	ErrTagCapReached  = errors.New("loto: tag cap reached")
	ErrTagNotMine     = errors.New("loto: tag not addressed to caller")
	ErrNoHostLock     = errors.New("loto: no host lock for tag")
	ErrTagTextTooLong = errors.New("loto: tag text exceeds 4096-byte cap")
)

func newTagID() string { return newID("t-") }

// InsertTag adds a tag bound to the host lock identified by
// (TargetCanonical, LockOwnerUUID, LockCreatedAt). The host-lock existence
// check and the per-host alive-tag cap (5) are enforced inside the same
// transaction as the INSERT so cap is TOCTOU-free.
func (s *Store) InsertTag(ctx context.Context, t NewTag) (string, error) {
	// Cheap language-level guard runs before opening a tx so oversized writes
	// never reach SQLite. CHECK constraint in schema.sql is the belt; this is
	// the suspenders that produces a typed error for callers.
	if len(t.Text) > tagTextMaxBytes {
		return "", ErrTagTextTooLong
	}
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	var hostExists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM locks
		WHERE target_canonical = ? AND owner_uuid = ? AND created_at = ?`,
		string(t.TargetCanonical), t.LockOwnerUUID, t.LockCreatedAt).Scan(&hostExists)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoHostLock
	}
	if err != nil {
		return "", err
	}

	var n int
	if err := tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM tags
		WHERE target_canonical = ? AND lock_owner_uuid = ? AND lock_created_at = ?
		  AND acked_at IS NULL`,
		string(t.TargetCanonical), t.LockOwnerUUID, t.LockCreatedAt).Scan(&n); err != nil {
		return "", err
	}
	if n >= tagCap {
		return "", ErrTagCapReached
	}

	id := newTagID()
	now := time.Now().UnixNano()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO tags(id, target_canonical, lock_owner_uuid, lock_created_at,
		                 tagger_uuid, text, created_at)
		VALUES(?,?,?,?,?,?,?)`,
		id, string(t.TargetCanonical), t.LockOwnerUUID, t.LockCreatedAt,
		t.TaggerUUID, t.Text, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListAliveForOwner returns pending tags whose host lock is currently held by
// ownerUUID and whose tagger is someone else (no self-echo). Deterministic
// order: created_at ASC, id ASC. Orphaned tags (host lock deleted) are filtered
// by the JOIN.
func (s *Store) ListAliveForOwner(ctx context.Context, ownerUUID domain.AgentUUID) ([]Tag, error) {
	owner := string(ownerUUID) // sqlite query arg crosses the untyped edge
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.target_canonical, t.lock_owner_uuid, t.lock_created_at,
		       t.tagger_uuid, t.text, t.created_at, t.acked_at
		FROM tags t
		JOIN locks l
		  ON l.target_canonical = t.target_canonical
		 AND l.owner_uuid       = t.lock_owner_uuid
		 AND l.created_at       = t.lock_created_at
		WHERE l.owner_uuid = ?
		  AND t.tagger_uuid <> ?
		  AND t.acked_at IS NULL
		ORDER BY t.created_at ASC, t.id ASC`, owner, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

// ListAliveByTargets is the batched form of ListAliveForTarget: one query for
// many canonical paths, grouped into a map. Callers that surface tags across a
// list of files (status, lock conflict) should prefer this — it folds an N+1
// loop into a single round-trip. Empty input returns an empty map.
func (s *Store) ListAliveByTargets(ctx context.Context, canonicals []domain.Canonical) (map[string][]Tag, error) {
	if len(canonicals) == 0 {
		return map[string][]Tag{}, nil
	}
	ss := make([]string, len(canonicals)) // sqlite query args cross the untyped edge
	for i, c := range canonicals {
		ss[i] = string(c)
	}
	placeholders, args := inClauseStrings(ss)
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.target_canonical, t.lock_owner_uuid, t.lock_created_at,`+ //nolint:gosec // G202 placeholders are '?' chars only, all data via args
		` t.tagger_uuid, t.text, t.created_at, t.acked_at`+
		` FROM tags t JOIN locks l`+
		`   ON l.target_canonical = t.target_canonical`+
		`  AND l.owner_uuid       = t.lock_owner_uuid`+
		`  AND l.created_at       = t.lock_created_at`+
		` WHERE t.target_canonical IN (`+placeholders+`) AND t.acked_at IS NULL`+
		` ORDER BY t.created_at ASC, t.id ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	all, err := scanTags(rows)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]Tag, len(canonicals))
	for _, t := range all {
		k := string(t.TargetCanonical) // map keyed by plain path to match Target.Canonical callers
		out[k] = append(out[k], t)
	}
	return out, nil
}

// ListAliveForTarget returns pending tags bound to the live lock on
// targetCanonical (if any). No self-tag filter — non-holder surfaces (status,
// lock conflict) show everyone's tags. Empty result when no live lock.
func (s *Store) ListAliveForTarget(ctx context.Context, targetCanonical domain.Canonical) ([]Tag, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT t.id, t.target_canonical, t.lock_owner_uuid, t.lock_created_at,
		       t.tagger_uuid, t.text, t.created_at, t.acked_at
		FROM tags t
		JOIN locks l
		  ON l.target_canonical = t.target_canonical
		 AND l.owner_uuid       = t.lock_owner_uuid
		 AND l.created_at       = t.lock_created_at
		WHERE t.target_canonical = ?
		  AND t.acked_at IS NULL
		ORDER BY t.created_at ASC, t.id ASC`, string(targetCanonical))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

func scanTags(rows *sql.Rows) ([]Tag, error) {
	var out []Tag
	for rows.Next() {
		var t Tag
		var canonical string // sqlite text column → domain.Canonical at the store boundary
		var acked sql.NullInt64
		if err := rows.Scan(&t.ID, &canonical, &t.LockOwnerUUID, &t.LockCreatedAt,
			&t.TaggerUUID, &t.Text, &t.CreatedAt, &acked); err != nil {
			return nil, err
		}
		t.TargetCanonical = domain.Canonical(canonical)
		if acked.Valid {
			v := acked.Int64
			t.AckedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ackClassifyHook fires between the UPDATE and the classifying SELECT inside
// Ack. Production no-op; a test seam to drive a concurrent mutation into the
// exact race window the immediate-mode tx must serialize against (loto-3c7y).
var ackClassifyHook = func() {}

// Ack marks one tag acked by byUUID. Idempotent: already-acked, orphaned, or
// unknown IDs return nil (no-op). Returns ErrTagNotMine when the tag exists
// but is addressed to a different holder.
func (s *Store) Ack(ctx context.Context, tagID string, byUUID domain.AgentUUID) error {
	by := string(byUUID) // sqlite query arg + owner comparison cross the untyped edge
	// The UPDATE and its 0-row classifying SELECT run in one immediate-mode tx
	// so the SELECT reads the same snapshot the UPDATE matched against. A
	// concurrent ReleaseLocks/gc (ackTagsForReleaseTx, gcTagsTx) takes the
	// write lock at its own BeginTx and serializes behind ours, so classify
	// can't see a reclaim+retag that landed mid-flight → deterministic result
	// (nil or a stable error) for the same logical state (loto-3c7y).
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	now := time.Now().UnixNano()
	res, err := tx.ExecContext(ctx, `
		UPDATE tags SET acked_at = ?
		WHERE id = ? AND lock_owner_uuid = ? AND acked_at IS NULL`,
		now, tagID, by)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return commitTxFn(tx)
	}

	ackClassifyHook()

	// 0 rows: either unknown id, already acked, or addressed to someone else.
	var owner sql.NullString
	var acked sql.NullInt64
	err = tx.QueryRowContext(ctx, `
		SELECT lock_owner_uuid, acked_at FROM tags WHERE id = ?`, tagID).Scan(&owner, &acked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // unknown id → no-op (edge #11)
	}
	if err != nil {
		return err
	}
	if owner.String != by {
		return ErrTagNotMine
	}
	return nil // already acked (edge #10)
}
