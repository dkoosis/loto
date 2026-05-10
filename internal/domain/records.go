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

type TagKind int

const (
	TagNote TagKind = iota
	TagSystem
)

type TagRecord struct {
	ID                string
	Target            Target
	Kind              TagKind
	Event             string
	AuthorUUID        string
	AddresseeUUID     string
	PreviousOwnerUUID string
	Intent            string
	CreatedAt         time.Time
	ExpiresAt         *time.Time
}
