package domain

import "errors"

var errLiveLockNoForce = errors.New("lock is live; --force required")

// AuthorizeBreak permits breaking a lock when force is set or the lock is stale
// under the evaluation context; otherwise a live lock requires --force.
func (c EvalContext) AuthorizeBreak(l LockRecord, force bool) error {
	if force {
		return nil
	}
	if c.IsStale(l) {
		return nil
	}
	return errLiveLockNoForce
}
