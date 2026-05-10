package loto

import (
	"context"
	"errors"
	"os"
	"time"
)

const (
	defaultPollInterval = 200 * time.Millisecond
	defaultMaxInterval  = 2 * time.Second
)

var errTTLNonPositive = errors.New("ttl must be positive")

// Acquire blocks until it can acquire a file lock on target, ctx is cancelled,
// or the context deadline is exceeded. It polls with exponential backoff
// capped at defaultMaxInterval.
func (l *LOTO) Acquire(ctx context.Context, agentID, intent, target string, opts ...TagOptions) (*ActiveLock, error) {
	return pollAcquire(ctx, "acquire: context cancelled", func() (*ActiveLock, error) {
		return l.TryFileLock(agentID, intent, target, opts...)
	})
}

// AcquirePath records a record-tier (acquire'd) hold on target with the
// given TTL and returns immediately. The hold survives process exit and
// remains authoritative until ttl elapses (lazy check; no daemon).
//
// On conflict (another agent holds the path via either foreground flock
// or an unexpired record-tier tag), returns *ErrHeld surfacing the holder.
// Same-agent re-acquire is idempotent and extends the TTL.
//
// The returned []*Reservation lists advisory reservations whose globs
// match target — the hook adapter's primary use case is to surface
// these to the editing agent before it touches the file.
func (l *LOTO) AcquirePath(agentID, intent, target string, ttl time.Duration, opts ...TagOptions) (*Tag, []*Reservation, error) {
	if ttl <= 0 {
		return nil, nil, &ErrSystem{Op: "acquire-path: ttl", Err: errTTLNonPositive}
	}

	globalLockPath, _ := l.globalPaths()
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return nil, nil, err
	}

	// Take global shared (consistency with TryFileLock — blocks during a global sweep).
	globalFile, err := os.OpenFile(globalLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, &ErrSystem{Op: "acquire-path: open global lock", Err: err}
	}
	defer globalFile.Close()
	if err := flockShared(globalFile); err != nil {
		if !isFlockContention(err) {
			return nil, nil, &ErrSystem{Op: "acquire-path: flock global", Err: err}
		}
		tag, _ := l.ReadGlobalTag()
		return nil, nil, &ErrHeld{Tag: tag, Kind: kindGlobal, Target: kindGlobal}
	}

	// Take file flock briefly for atomic tag write.
	fileFile, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, nil, &ErrSystem{Op: "acquire-path: open file lock", Err: err}
	}
	defer fileFile.Close()
	if err := flockExclusive(fileFile); err != nil {
		if !isFlockContention(err) {
			return nil, nil, &ErrSystem{Op: "acquire-path: flock file", Err: err}
		}
		// Foreground holder has the flock — surface their identity.
		tag, _ := l.ReadTag(target)
		return nil, nil, &ErrHeld{Tag: tag, Kind: kindFile, Target: target}
	}

	// Record-tier guard: existing tag from a different agent, still authoritative?
	if existing, _ := l.ReadTag(target); existing != nil &&
		existing.IsRecordTier() && existing.AgentID != agentID {
		return nil, nil, &ErrHeld{Tag: existing, Kind: kindFile, Target: target}
	}

	// Compose effective TagOptions: the explicit ttl wins.
	effOpts := TagOptions{TTL: ttl}
	if len(opts) > 0 && opts[0].TTL > effOpts.TTL {
		effOpts.TTL = opts[0].TTL
	}
	tag := l.newTag(agentID, intent, target, kindFile, effOpts)
	if err := l.writeTagAtomic(fileTagPath, tag); err != nil {
		return nil, nil, err
	}

	// Advisory: matching reservations for the hook adapter's pre-write signal.
	conflicts, _ := l.ConflictingReservations(target)

	// Both flocks released by deferred Close. Tag carries authority via TTL.
	return &tag, conflicts, nil
}
func (l *LOTO) AcquireGlobal(ctx context.Context, agentID, intent string, opts ...TagOptions) (*ActiveLock, error) {
	return pollAcquire(ctx, "acquire-global: context cancelled", func() (*ActiveLock, error) {
		return l.TryGlobalLock(agentID, intent, opts...)
	})
}

// pollAcquire retries try with exponential backoff until it succeeds, returns
// a non-contention error, or ctx is done. On context cancellation it returns
// the last ErrHeld observed (the actual blocker) so callers see the holder,
// not a synthetic "context cancelled" error. cancelOp is used only as a
// fallback ErrSystem op label when no ErrHeld was ever observed.
func pollAcquire(ctx context.Context, cancelOp string, try func() (*ActiveLock, error)) (*ActiveLock, error) {
	interval := defaultPollInterval
	var lastHeld *ErrHeld
	for {
		lock, err := try()
		if err == nil {
			return lock, nil
		}
		var held *ErrHeld
		if !errors.As(err, &held) {
			return nil, err
		}
		lastHeld = held

		select {
		case <-ctx.Done():
			if lastHeld != nil {
				return nil, lastHeld
			}
			return nil, &ErrSystem{Op: cancelOp, Err: ctx.Err()}
		case <-time.After(interval):
		}

		interval *= 2
		if interval > defaultMaxInterval {
			interval = defaultMaxInterval
		}
	}
}
