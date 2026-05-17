package domain

import (
	"errors"
	"time"
)

var ErrLiveLockNoForce = errors.New("lock is live; --force required")

func AuthorizeBreak(l LockRecord, force bool, now time.Time, thisHost string, live PidLiveProbe) error {
	if force {
		return nil
	}
	if IsStale(l, now, thisHost, live) {
		return nil
	}
	return ErrLiveLockNoForce
}
