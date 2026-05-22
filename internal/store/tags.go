package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// Tag is the persisted form of a `loto tag` annotation. Lifetime is parasitic
// on the host lock identified by (TargetCanonical, LockOwnerUUID, LockCreatedAt);
// when the host lock disappears the tag becomes orphaned and is filtered from
// alive lists at read time, then hard-deleted by doctor --repair.
type Tag struct {
	ID, TargetCanonical, LockOwnerUUID, TaggerUUID, Text string
	LockCreatedAt, CreatedAt                             int64
	AckedAt                                              *int64
}

// NewTag is the InsertTag input. Caller resolves the host-lock triple
// (target, owner, created_at) from the live `locks` row before calling.
type NewTag struct {
	TargetCanonical, LockOwnerUUID, TaggerUUID, Text string
	LockCreatedAt                                    int64
}

const tagCap = 5

var (
	ErrTagCapReached = errors.New("loto: tag cap reached")
	ErrTagNotMine    = errors.New("loto: tag not addressed to caller")
	ErrNoHostLock    = errors.New("loto: no host lock for tag")
)

func newTagID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Matches newEventID: crypto/rand failing is catastrophic; refuse to
		// mint predictable IDs.
		panic("crypto/rand: " + err.Error())
	}
	return "t-" + hex.EncodeToString(b[:])
}

// InsertTag adds a tag bound to the host lock identified by
// (TargetCanonical, LockOwnerUUID, LockCreatedAt). The host-lock existence
// check and the per-host alive-tag cap (5) are enforced inside the same
// transaction as the INSERT so cap is TOCTOU-free.
func (s *Store) InsertTag(ctx context.Context, t NewTag) (string, error) {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	var hostExists int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM locks
		WHERE target_canonical = ? AND owner_uuid = ? AND created_at = ?`,
		t.TargetCanonical, t.LockOwnerUUID, t.LockCreatedAt).Scan(&hostExists)
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
		t.TargetCanonical, t.LockOwnerUUID, t.LockCreatedAt).Scan(&n); err != nil {
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
		id, t.TargetCanonical, t.LockOwnerUUID, t.LockCreatedAt,
		t.TaggerUUID, t.Text, now); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListAliveForHolder returns pending tags whose host lock is currently held by
// ownerUUID and whose tagger is someone else (no self-echo). Deterministic
// order: created_at ASC, id ASC. Orphaned tags (host lock deleted) are filtered
// by the JOIN.
func (s *Store) ListAliveForHolder(ctx context.Context, ownerUUID string) ([]Tag, error) {
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
		ORDER BY t.created_at ASC, t.id ASC`, ownerUUID, ownerUUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

// ListAliveForTarget returns pending tags bound to the live lock on
// targetCanonical (if any). No self-tag filter — non-holder surfaces (status,
// lock conflict) show everyone's tags. Empty result when no live lock.
func (s *Store) ListAliveForTarget(ctx context.Context, targetCanonical string) ([]Tag, error) {
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
		ORDER BY t.created_at ASC, t.id ASC`, targetCanonical)
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
		var acked sql.NullInt64
		if err := rows.Scan(&t.ID, &t.TargetCanonical, &t.LockOwnerUUID, &t.LockCreatedAt,
			&t.TaggerUUID, &t.Text, &t.CreatedAt, &acked); err != nil {
			return nil, err
		}
		if acked.Valid {
			v := acked.Int64
			t.AckedAt = &v
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Ack marks one tag acked by byUUID. Idempotent: already-acked, orphaned, or
// unknown IDs return nil (no-op). Returns ErrTagNotMine when the tag exists
// but is addressed to a different holder.
func (s *Store) Ack(ctx context.Context, tagID, byUUID string) error {
	now := time.Now().UnixNano()
	res, err := s.db.ExecContext(ctx, `
		UPDATE tags SET acked_at = ?
		WHERE id = ? AND lock_owner_uuid = ? AND acked_at IS NULL`,
		now, tagID, byUUID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}

	// 0 rows: either unknown id, already acked, or addressed to someone else.
	var owner sql.NullString
	var acked sql.NullInt64
	err = s.db.QueryRowContext(ctx, `
		SELECT lock_owner_uuid, acked_at FROM tags WHERE id = ?`, tagID).Scan(&owner, &acked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // unknown id → no-op (edge #11)
	}
	if err != nil {
		return err
	}
	if owner.String != byUUID {
		return ErrTagNotMine
	}
	return nil // already acked (edge #10)
}
