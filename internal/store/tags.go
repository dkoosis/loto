package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"loto/internal/domain"
)

const tagCols = `target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at`

func (s *Store) AddTag(ctx context.Context, tg domain.TagRecord) (string, error) {
	if tg.ID == "" {
		tg.ID = newTagID(tg.AuthorUUID, tg.CreatedAt, tg.Intent)
	}
	var expiresNs sql.NullInt64
	if tg.ExpiresAt != nil {
		expiresNs = sql.NullInt64{Valid: true, Int64: tg.ExpiresAt.UnixNano()}
	}
	var addressee, prev, event sql.NullString
	if tg.AddresseeUUID != "" {
		addressee = sql.NullString{Valid: true, String: tg.AddresseeUUID}
	}
	if tg.PreviousOwnerUUID != "" {
		prev = sql.NullString{Valid: true, String: tg.PreviousOwnerUUID}
	}
	if tg.Event != "" {
		event = sql.NullString{Valid: true, String: tg.Event}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO tags(`+tagCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		tg.Target.Canonical, kindString(tg.Target.Kind), tg.ID, kindTagString(tg.Kind), event,
		tg.AuthorUUID, addressee, prev, tg.Intent, tg.CreatedAt.UnixNano(), expiresNs)
	if err != nil {
		return "", err
	}
	return tg.ID, nil
}

func (s *Store) RemoveTag(ctx context.Context, t domain.Target, id, byAgent string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM tags WHERE target_canonical = ? AND id = ? AND author_uuid = ?`, t.Canonical, id, byAgent)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("not author or tag missing")
	}
	return nil
}

func (s *Store) TagsOnTarget(ctx context.Context, t domain.Target) ([]domain.TagRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tagCols+` FROM tags WHERE target_canonical = ? ORDER BY created_at, id`, t.Canonical)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

func (s *Store) ListAllTags(ctx context.Context) ([]domain.TagRecord, error) {
	now := time.Now().UnixNano()
	rows, err := s.db.QueryContext(ctx, `SELECT `+tagCols+` FROM tags WHERE expires_at IS NULL OR expires_at > ? ORDER BY created_at, id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTags(rows)
}

func scanTags(rows *sql.Rows) ([]domain.TagRecord, error) {
	var out []domain.TagRecord
	for rows.Next() {
		var (
			tg         domain.TagRecord
			canonical  string
			tgtKindStr string
			kindStr    string
			event      sql.NullString
			addressee  sql.NullString
			prev       sql.NullString
			createdNs  int64
			expiresNs  sql.NullInt64
		)
		if err := rows.Scan(&canonical, &tgtKindStr, &tg.ID, &kindStr, &event, &tg.AuthorUUID, &addressee, &prev, &tg.Intent, &createdNs, &expiresNs); err != nil {
			return nil, err
		}
		tg.Target = domain.Target{Canonical: canonical, Kind: parseKind(tgtKindStr)}
		tg.Kind = parseTagKind(kindStr)
		if event.Valid {
			tg.Event = event.String
		}
		if addressee.Valid {
			tg.AddresseeUUID = addressee.String
		}
		if prev.Valid {
			tg.PreviousOwnerUUID = prev.String
		}
		tg.CreatedAt = time.Unix(0, createdNs).UTC()
		if expiresNs.Valid {
			t := time.Unix(0, expiresNs.Int64).UTC()
			tg.ExpiresAt = &t
		}
		out = append(out, tg)
	}
	return out, rows.Err()
}

func kindTagString(k domain.TagKind) string {
	if k == domain.TagSystem {
		return "system"
	}
	return "note"
}

func parseTagKind(s string) domain.TagKind {
	if s == "system" {
		return domain.TagSystem
	}
	return domain.TagNote
}
