//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sync"
	"syscall"
	"time"
)

// ErrFlockTimeout is returned when acquireOpFlock cannot take the project
// op-flock within LOTO_FLOCK_TIMEOUT (default 30s).
var ErrFlockTimeout = errors.New("loto: op-flock acquire timed out")

const (
	flockPollInitial  = 25 * time.Millisecond
	flockPollMax      = 250 * time.Millisecond
	flockJitterFactor = 0.25 // ±25% — desync wakeups when many agents share an op-flock
	flockNoticeAfter  = 250 * time.Millisecond
	flockDefaultLimit = 30 * time.Second
)

type opFlock struct {
	f *os.File
}

func (h *opFlock) release() {
	if h == nil || h.f == nil {
		return
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
}

// acquireOpFlock takes a project-wide exclusive flock on path with a bounded
// wait. Polls with LOCK_NB every 50ms; emits a one-shot wait notice on stderrW
// after 250ms cumulative wait; returns ErrFlockTimeout after LOTO_FLOCK_TIMEOUT
// (default 30s). Kernel releases on process exit.
//
// stderrW is passed in (rather than read from a package global) so concurrent
// callers under `go test -race` cannot data-race on a shared writer.
func acquireOpFlock(ctx context.Context, path string, stderrW io.Writer) (*opFlock, error) {
	limit := flockLimitFromEnv()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open op-flock: %w", err)
	}
	var noticed sync.Once
	deadline := time.Now().Add(limit)
	start := time.Now()
	backoff := flockPollInitial
	for {
		// Notice check sits at the top so a long-cumulative wait that ends in a
		// successful try still emits — the user was waiting, even if the next
		// flock call wins. Otherwise the wakeup→success path skips notice and
		// the test (and CLI UX) gets nondeterministic feedback.
		maybeEmitWaitNotice(stderrW, start, &noticed)
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &opFlock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock op-flock: %w", err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrFlockTimeout
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(jitter(backoff)):
		}
		backoff = nextBackoff(backoff)
	}
}

// nextBackoff doubles up to flockPollMax. Cheap to compute, no overshoot risk
// on long waits — once at the cap we stay there.
func nextBackoff(d time.Duration) time.Duration {
	if d >= flockPollMax {
		return flockPollMax
	}
	d *= 2
	if d > flockPollMax {
		d = flockPollMax
	}
	return d
}

// jitter applies ±flockJitterFactor randomization so concurrent agents woken
// at the same instant (release of a shared op-flock) don't re-collide on the
// next syscall. Uniform in [d*(1-f), d*(1+f)].
func jitter(d time.Duration) time.Duration {
	delta := time.Duration(float64(d) * flockJitterFactor * (rand.Float64()*2 - 1)) //nolint:gosec // jitter for poll desync, not security
	return d + delta
}

func flockLimitFromEnv() time.Duration {
	if s := os.Getenv("LOTO_FLOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			return d
		}
	}
	return flockDefaultLimit
}

func maybeEmitWaitNotice(stderrW io.Writer, start time.Time, noticed *sync.Once) {
	if stderrW == nil || time.Since(start) < flockNoticeAfter {
		return
	}
	noticed.Do(func() { fmt.Fprintln(stderrW, "✓ waiting flock=lock-op") })
}
