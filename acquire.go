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
	interval := defaultPollInterval
	for {
		lock, err := l.TryFileLock(agentID, intent, target, opts...)
		if err == nil {
			return lock, nil
		}
		var held *ErrHeld
		if !errors.As(err, &held) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, &ErrSystem{Op: "acquire: context cancelled", Err: ctx.Err()}
		case <-time.After(interval):
		}

		interval *= 2
		if interval > defaultMaxInterval {
			interval = defaultMaxInterval
		}
	}
}

// AcquireGlobal blocks until the global lock is acquired or ctx is done.
func (l *LOTO) AcquireGlobal(ctx context.Context, agentID, intent string, opts ...TagOptions) (*ActiveLock, error) {
	interval := defaultPollInterval
	for {
		lock, err := l.TryGlobalLock(agentID, intent, opts...)
		if err == nil {
			return lock, nil
		}
		var held *ErrHeld
		if !errors.As(err, &held) {
			return nil, err
		}

		select {
		case <-ctx.Done():
			return nil, &ErrSystem{Op: "acquire-global: context cancelled", Err: ctx.Err()}
		case <-time.After(interval):
		}

		interval *= 2
		if interval > defaultMaxInterval {
			interval = defaultMaxInterval
		}
	}
}
