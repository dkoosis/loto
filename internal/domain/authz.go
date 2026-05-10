package domain

import (
	"errors"
	"time"
)

var (
	ErrNotOwner        = errors.New("not the lock owner")
	ErrLiveLockNoForce = errors.New("lock is live; --force required")
)

func AuthorizeUnlock(l LockRecord, byAgent string) error {
	if l.OwnerUUID != byAgent {
		return ErrNotOwner
	}
	return nil
}

func AuthorizeBreak(l LockRecord, byAgent string, force bool, now time.Time, thisHost string, live PidLiveProbe) error {
	if force {
		return nil
	}
	if IsStale(l, now, thisHost, live) {
		return nil
	}
	return ErrLiveLockNoForce
}
