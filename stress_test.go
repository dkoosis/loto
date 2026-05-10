//go:build stress

package loto

import (
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestStressConcurrentAgents: N goroutines, each playing a fake agent that
// loops through acquire/release/reserve/unreserve operations on a shared
// pool of targets. The test asserts:
//
//   - zero system errors (only ErrHeld/ErrNotMine/ErrInvalidGlob are allowed)
//   - after every agent stops and ReleaseAllMine fires, doctor reports clean
//
// Build-tag `stress` keeps it out of the default `make audit` run; invoke via
// `make stress`. Bead loto-8ru.
func TestStressConcurrentAgents(t *testing.T) {
	const (
		numAgents      = 16
		opsPerAgent    = 200
		numTargetFiles = 8
		numPatterns    = 4
		seedBase       = 0xC0FFEE
	)

	base := t.TempDir()
	l, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Targets and patterns shared across agents — deliberate contention.
	dir := t.TempDir()
	targets := make([]string, numTargetFiles)
	for i := range targets {
		targets[i] = filepath.Join(dir, fmt.Sprintf("file-%02d.go", i))
	}
	patterns := []string{"a/**", "b/**", "c/**", "d/**"}[:numPatterns]

	var sysErrs atomic.Int64
	var (
		errMu      sync.Mutex
		sampleErrs []error
	)
	logErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if len(sampleErrs) < 5 {
			sampleErrs = append(sampleErrs, err)
		}
	}
	var wg sync.WaitGroup
	wg.Add(numAgents)

	for i := range numAgents {
		go func() {
			defer wg.Done()
			agentID := fmt.Sprintf("stress-agent-%02d", i)
			rng := rand.New(rand.NewSource(int64(seedBase) + int64(i)))
			for range opsPerAgent {
				if err := runStressOp(l, agentID, targets, patterns, rng); err != nil {
					sysErrs.Add(1)
					logErr(err)
				}
			}
			// Cleanup: releases everything this agent owns.
			if _, errs := l.ReleaseAllMine(agentID); len(errs) > 0 {
				t.Errorf("%s: ReleaseAllMine errs=%v", agentID, errs)
				sysErrs.Add(int64(len(errs)))
			}
		}()
	}
	wg.Wait()

	if got := sysErrs.Load(); got != 0 {
		t.Fatalf("zero system errors expected; got %d. samples=%v", got, sampleErrs)
	}

	// Zombie drift check: after every agent stops, doctor must be clean.
	// Poll briefly to absorb fsnotify lag and the rare retried release path.
	var rep *DoctorReport
	deadline := time.Now().Add(2 * time.Second)
	for {
		var derr error
		rep, derr = l.Doctor("stress-checker", DoctorCheck)
		if derr != nil {
			t.Fatalf("Doctor: %v", derr)
		}
		if rep.Clean || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !rep.Clean {
		for _, f := range rep.Findings {
			t.Logf("doctor finding: class=%s path=%s detail=%s", f.Class, f.Path, f.Detail)
		}
		t.Fatalf("zombie drift: %d findings", len(rep.Findings))
	}

	// Reservation drift: a concurrent Reserve race between agents can leave
	// a small number of tags whose last writer's release missed the file
	// (saw a different agent's content at read-time, skipped). Cap at
	// numPatterns — worst case one stuck per shared pattern. Tracked as a
	// known follow-up race; gauntlet's primary value is the lock-error
	// assertion above.
	resv, err := l.ListReservations()
	if err != nil {
		t.Fatalf("ListReservations: %v", err)
	}
	if len(resv) > numPatterns {
		for _, r := range resv {
			t.Logf("lingering reservation: agent=%s pattern=%s created=%s", r.AgentID, r.Pattern, r.CreatedAt)
		}
		t.Fatalf("reservation drift: %d remaining > cap=%d", len(resv), numPatterns)
	}
	if len(resv) > 0 {
		t.Logf("known race: %d residual reservations (cap %d).", len(resv), numPatterns)
	}
}

// runStressOp picks one of the loto operations at random and runs it.
// Returns the unexpected error (or nil if the operation result was acceptable).
func runStressOp(l *LOTO, agentID string, targets, patterns []string, rng *rand.Rand) error {
	switch rng.Intn(6) {
	case 0: // acquire path
		t := targets[rng.Intn(len(targets))]
		_, _, err := l.AcquirePath(agentID, "stress", t, time.Minute)
		return checkErr(err)
	case 1: // release path
		t := targets[rng.Intn(len(targets))]
		return checkErr(l.ReleasePath(agentID, t))
	case 2: // try file lock + immediate unlock
		t := targets[rng.Intn(len(targets))]
		lock, err := l.TryFileLock(agentID, "stress", t)
		if e := checkErr(err); e != nil {
			return e
		}
		if lock != nil {
			_ = lock.Unlock()
		}
		return nil
	case 3: // reserve
		p := patterns[rng.Intn(len(patterns))]
		_, err := l.Reserve(agentID, "stress", p, time.Minute)
		return checkErr(err)
	case 4: // unreserve
		p := patterns[rng.Intn(len(patterns))]
		return checkErr(l.Unreserve(p))
	default: // list reservations (read-only)
		_, err := l.ListReservations()
		return checkErr(err)
	}
}

// checkErr returns nil for expected coordination outcomes (held, not-mine,
// invalid-glob); returns the original error for anything else.
func checkErr(err error) error {
	if err == nil {
		return nil
	}
	var held *ErrHeld
	if errors.As(err, &held) {
		return nil
	}
	var notMine *ErrNotMine
	if errors.As(err, &notMine) {
		return nil
	}
	if errors.Is(err, ErrInvalidGlob) {
		return nil
	}
	return err
}
