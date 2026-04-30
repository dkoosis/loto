package loto

import (
	"context"
	"errors"
	"time"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	defaultMaxInterval  = 2 * time.Second
)

// Acquire blocks until it can acquire a file lock on target, ctx is cancelled,
// or the context deadline is exceeded. It polls with exponential backoff
// capped at defaultMaxInterval.
func (l *LOTO) Acquire(ctx context.Context, agentID, intent, target string, opts ...TagOptions) (*ActiveLock, error) {
	return pollAcquire(ctx, "acquire: context cancelled", func() (*ActiveLock, error) {
		return l.TryFileLock(agentID, intent, target, opts...)
	})
}

// AcquireGlobal blocks until the global lock is acquired or ctx is done.
func (l *LOTO) AcquireGlobal(ctx context.Context, agentID, intent string, opts ...TagOptions) (*ActiveLock, error) {
	return pollAcquire(ctx, "acquire-global: context cancelled", func() (*ActiveLock, error) {
		return l.TryGlobalLock(agentID, intent, opts...)
	})
}

// pollAcquire retries try with exponential backoff until it succeeds, returns
// a non-contention error, or ctx is done. cancelOp labels the ErrSystem
// returned on context cancellation.
func pollAcquire(ctx context.Context, cancelOp string, try func() (*ActiveLock, error)) (*ActiveLock, error) {
	interval := defaultPollInterval
	for {
		lock, err := try()
		if err == nil {
			return lock, nil
		}
		var held *ErrHeld
		if !errors.As(err, &held) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, &ErrSystem{Op: cancelOp, Err: ctx.Err()}
		case <-time.After(interval):
		}

		interval *= 2
		if interval > defaultMaxInterval {
			interval = defaultMaxInterval
		}
	}
}
