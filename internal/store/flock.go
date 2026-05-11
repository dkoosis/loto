//go:build unix

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// ErrFlockTimeout is returned when acquireOpFlock cannot take the project
// op-flock within LOTO_FLOCK_TIMEOUT (default 30s).
var ErrFlockTimeout = errors.New("loto: op-flock acquire timed out")

const (
	flockPollInterval = 50 * time.Millisecond
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
func acquireOpFlock(path string, stderrW io.Writer) (*opFlock, error) {
	limit := flockDefaultLimit
	if s := os.Getenv("LOTO_FLOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			limit = d
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open op-flock: %w", err)
	}
	var noticed sync.Once
	deadline := time.Now().Add(limit)
	start := time.Now()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &opFlock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock op-flock: %w", err)
		}
		if stderrW != nil && time.Since(start) >= flockNoticeAfter {
			noticed.Do(func() { fmt.Fprintln(stderrW, "ℹ waiting flock=lock-op") })
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrFlockTimeout
		}
		time.Sleep(flockPollInterval)
	}
}
