package domain

import "time"

type LockRecord struct {
	Target      Target
	OwnerUUID   string
	SessionUUID string
	Intent      string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	Host        string
	PID         int
	Branch      string
}

// Event is an append-only audit row. SubjectUUID is the affected agent (for
// lock_broken / lock_reclaimed_stale); empty otherwise.
type Event struct {
	ID          string
	Target      Target
	Kind        string
	ActorUUID   string
	SubjectUUID string
	Reason      string
	CreatedAt   time.Time
}
