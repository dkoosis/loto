package store

import (
	"context"
	"time"

	"loto/internal/domain"
)

// LatestEventByKindActor returns the newest event of `kind` written by
// `actorUUID` at or after `since`, and whether one exists.
//
// It is the read behind I1's override counter (enforcement-design §10b row
// 1): `loto claim .` asks whether this same owner was refused a ref
// transition in the recent past, and writes ref_refused_overridden only if
// the answer is yes. Scoped by actor because an override is a statement about
// the refusal the SAME session just hit — a peer's refusal a minute earlier
// says nothing about why this owner is claiming the checkout.
//
// ‡ Bounded by events retention (EventsRetentionMaxRows / EventsRetentionAge),
// like every other events read. A refusal rotated out reads as "no refusal",
// which loses a counter row and never miscounts one — the failure direction
// the ratio can survive.
func (s *Store) LatestEventByKindActor(ctx context.Context, kind, actorUUID string, since time.Time) (domain.Event, bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventCols+` FROM events
		  WHERE event_kind = ? AND actor_uuid = ? AND created_at >= ?
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		kind, actorUUID, since.UnixNano())
	if err != nil {
		return domain.Event{}, false, err
	}
	defer rows.Close()
	evs, err := scanEvents(rows)
	if err != nil || len(evs) == 0 {
		return domain.Event{}, false, err
	}
	return evs[0], true, nil
}
