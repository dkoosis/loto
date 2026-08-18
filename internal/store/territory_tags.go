package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// territoryTagsDDL is the territory_tags table shape, duplicated from
// schema.sql per the claimsDDL precedent: a pending ensureFn must be able to
// apply itself, even though the usual delivery path is migrate's schemaSQL
// pass. No user_version bump — bumping trips the move-aside path and would
// destroy live locks (loto-kwlp).
const territoryTagsDDL = `
CREATE TABLE IF NOT EXISTS territory_tags (
  id            TEXT PRIMARY KEY,
  path_prefix   TEXT NOT NULL,
  tagger_uuid   TEXT NOT NULL,
  text          TEXT NOT NULL CHECK (length(text) <= 4096),
  created_at    INTEGER NOT NULL,
  expires_at    INTEGER NOT NULL,
  acked_at      INTEGER,
  acked_by_uuid TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_territory_tags_live ON territory_tags(expires_at, acked_at);`

// TerritoryTag is a note pinned to repo territory — an unlocked file path or a
// directory prefix — for the agent who arrives there next.
//
// ‡ It is NOT a message. No addressee, no per-reader state, no delivery queue,
// no "did anyone pick this up" query. It reaches its reader by being pinned to
// ground the reader walks onto (loto-z3y1, decision 90b671928bbe). Mail retired
// entirely; this re-homes its one surviving use case — leave word for an agent
// that is not running yet — in loto's own vocabulary.
//
// Unlike Tag, whose lifetime is parasitic on a host lock, a territory tag is a
// TTL lease exactly like ClaimRecord: Expired is the sole staleness authority,
// no PID, no liveness refinement. That is deliberate reuse — it gives the
// codebase one staleness model for territory-shaped state rather than two.
type TerritoryTag struct {
	ID, PathPrefix, TaggerUUID, Text string
	CreatedAt, ExpiresAt             int64
	AckedAt                          *int64
	// AckedByUUID is "" until acked. ANY agent may ack (loto-z3y1 D4): the
	// reader who decides NOT to take the territory is exactly the reader the
	// note served, and requiring a lock would leave that agent unable to close
	// it. Recording who acked keeps the audit trail without creating
	// per-agent read state, which would be an inbox.
	AckedByUUID string
}

// Expired reports whether the note's TTL has lapsed. Mirrors
// domain.ClaimRecord.Expired — TTL is the only authority.
func (n TerritoryTag) Expired(nowNs int64) bool { return nowNs >= n.ExpiresAt }

// Live reports whether the note should still be surfaced: unacked and unexpired.
func (n TerritoryTag) Live(nowNs int64) bool { return n.AckedAt == nil && !n.Expired(nowNs) }

// NewTerritoryTag is the InsertTerritoryTag input. The caller has already
// canonicalized PathPrefix through domain.CanonicalizePrefix, so a file path
// and a directory prefix arrive here indistinguishable — which is the point
// (loto-z3y1 D8: PrefixOverlaps returns true on equality, so a file path is
// just a prefix matching exactly one target).
type NewTerritoryTag struct {
	PathPrefix, TaggerUUID, Text string
	ExpiresAt                    int64
}

const (
	// territoryTagPrefixCap bounds live notes at one exact prefix, matching
	// tagCap's reasoning and its value.
	territoryTagPrefixCap = 5
	// territoryTagRepoCap is the second half of the anti-accumulation
	// guarantee: the per-prefix cap alone lets a loop bomb 100 sibling
	// prefixes and call it 100 different territories.
	territoryTagRepoCap = 50
	// territoryTagMaxTTL is the anti-inbox guarantee expressed as a system
	// property rather than caller discipline: no note can outlive a month,
	// whatever TTL the caller passes.
	territoryTagMaxTTL = 30 * 24 * time.Hour
)

// ErrTerritoryTagTTLTooLong reports a requested TTL beyond territoryTagMaxTTL.
// Typed rather than clamped: silently shortening a caller's stated intent is
// the kind of helpfulness that gets discovered three weeks later.
var ErrTerritoryTagTTLTooLong = errors.New("loto: territory tag TTL exceeds the 30d cap")

func newTerritoryTagID() string { return newID("tt-") }

// InsertTerritoryTag pins a note to territory. No host-lock probe — there is
// nothing to bind to, and that absence is the whole feature.
//
// Both caps and the TTL ceiling are enforced inside the same transaction as
// the INSERT, so they are TOCTOU-free the way InsertTag's per-host cap is.
func (s *Store) InsertTerritoryTag(ctx context.Context, n NewTerritoryTag) (string, error) {
	// Cheap language-level guards run before the tx so a rejected write never
	// reaches SQLite. The CHECK constraint in schema.sql is the belt; these are
	// the suspenders that produce typed errors (gh#129 precedent).
	if len(n.Text) > tagTextMaxBytes {
		return "", ErrTagTextTooLong
	}
	now := time.Now()
	if n.ExpiresAt > now.Add(territoryTagMaxTTL).UnixNano() {
		return "", ErrTerritoryTagTTLTooLong
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return "", err
	}
	defer cleanup()

	nowNs := now.UnixNano()
	var atPrefix, repoWide int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM territory_tags WHERE path_prefix = ? AND acked_at IS NULL AND expires_at > ?`,
		n.PathPrefix, nowNs).Scan(&atPrefix); err != nil {
		return "", err
	}
	if atPrefix >= territoryTagPrefixCap {
		return "", ErrTagCapReached
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM territory_tags WHERE acked_at IS NULL AND expires_at > ?`,
		nowNs).Scan(&repoWide); err != nil {
		return "", err
	}
	if repoWide >= territoryTagRepoCap {
		return "", ErrTagCapReached
	}

	id := newTerritoryTagID()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO territory_tags (id, path_prefix, tagger_uuid, text, created_at, expires_at)`+
			` VALUES (?, ?, ?, ?, ?, ?)`,
		id, n.PathPrefix, n.TaggerUUID, n.Text, nowNs, n.ExpiresAt); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return id, nil
}

// ListTerritoryTags returns EVERY row, expired and acked included. Staleness is
// a display-time judgment, exactly as it is for ListClaims — the store reports
// what is written and the caller decides what is worth showing. Deterministic
// order (path_prefix, created_at, id) so the same DB renders byte-identically.
func (s *Store) ListTerritoryTags(ctx context.Context) ([]TerritoryTag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path_prefix, tagger_uuid, text, created_at, expires_at, acked_at, acked_by_uuid`+
			` FROM territory_tags ORDER BY path_prefix, created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTerritoryTags(rows)
}

// ExpiredTerritoryTags returns the notes that lapsed with nobody having acked
// them — the ones doctor reports before --repair sweeps them (loto-z3y1 D2).
//
// ‡ Unacked only. A note that was read and acked, then aged out, was delivered;
// reporting it would bury the genuinely undelivered ones. A note that expired
// unread is the mail-lost failure returning, and doctor is where loto says so.
func (s *Store) ExpiredTerritoryTags(ctx context.Context, nowNs int64) ([]TerritoryTag, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, path_prefix, tagger_uuid, text, created_at, expires_at, acked_at, acked_by_uuid`+
			` FROM territory_tags WHERE acked_at IS NULL AND expires_at <= ?`+
			` ORDER BY path_prefix, created_at, id`, nowNs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTerritoryTags(rows)
}

func scanTerritoryTags(rows *sql.Rows) ([]TerritoryTag, error) {
	var out []TerritoryTag
	for rows.Next() {
		var n TerritoryTag
		var acked sql.NullInt64
		if err := rows.Scan(&n.ID, &n.PathPrefix, &n.TaggerUUID, &n.Text,
			&n.CreatedAt, &n.ExpiresAt, &acked, &n.AckedByUUID); err != nil {
			return nil, err
		}
		if acked.Valid {
			v := acked.Int64
			n.AckedAt = &v
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// AckTerritoryTag dismisses a note. Idempotent: an unknown id and an
// already-acked note both answer nil.
//
// ‡ No ErrTagNotMine branch, unlike Ack. Any agent may ack (D4), so there is no
// ownership to classify and therefore no classifying SELECT — one UPDATE says
// everything. A later "only the holder may ack" tightening would have to delete
// TestAckTerritoryTagByAnyAgent to land, which is the point.
func (s *Store) AckTerritoryTag(ctx context.Context, id, by string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE territory_tags SET acked_at = ?, acked_by_uuid = ? WHERE id = ? AND acked_at IS NULL`,
		time.Now().UnixNano(), by, id)
	return err
}

// gcTerritoryTagsTx sweeps expired notes and long-acked ones inside
// DoctorRepair's tx, beside gcTagsTx and gcClaimsTx.
//
// `expires_at <= now` is the exact complement of the live probe's
// `expires_at > now`, so a note is never both surfaced and sweepable. Acked
// rows linger for tagsRetentionAge (7d) so the audit window stays uniform with
// events and held tags.
func gcTerritoryTagsTx(ctx context.Context, tx *sql.Tx, now time.Time) error {
	nowNs := now.UnixNano()
	_, err := tx.ExecContext(ctx,
		`DELETE FROM territory_tags WHERE expires_at <= ? OR (acked_at IS NOT NULL AND acked_at < ?)`,
		nowNs, now.Add(-tagsRetentionAge).UnixNano())
	return err
}

// ensureTerritoryTagsTable adds the table to a DB stamped before this feature.
// Follows ensureClaimsTable verbatim: probe sqlite_master, apply the DDL,
// never bump user_version.
func ensureTerritoryTagsTable(ctx context.Context, db sqlExecQuerier, apply bool) (bool, error) {
	return ensureTableBySentinelName(ctx, db, apply, "territory_tags", territoryTagsDDL)
}
