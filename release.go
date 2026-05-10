package loto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// flockExclusiveWithRetry takes an exclusive flock, retrying on contention
// with exponential backoff up to deadline. Used by paths that legitimately
// race on the same lock-file under concurrent operation (release vs. acquire
// vs. lazy-GC) — bead loto-8ru.
func flockExclusiveWithRetry(f *os.File, deadline time.Duration) error {
	end := time.Now().Add(deadline)
	wait := 1 * time.Millisecond
	for {
		err := flockExclusive(f)
		if err == nil {
			return nil
		}
		if !isFlockContention(err) {
			return err
		}
		if time.Now().After(end) {
			return err
		}
		time.Sleep(wait)
		if wait < 16*time.Millisecond {
			wait *= 2
		}
	}
}

// tagStatus is the outcome of attempting to load a tag file from disk.
type tagStatus int

const (
	tagOK      tagStatus = iota // file present, JSON parsed
	tagMissing                  // file absent or unreadable
	tagCorrupt                  // file present but JSON did not parse
)

// loadTag reads and unmarshals a tag file. The returned tagStatus distinguishes
// missing-or-unreadable from corrupt-JSON so callers can treat them differently.
func loadTag(path string) (Tag, tagStatus) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Tag{}, tagMissing
	}
	var t Tag
	if json.Unmarshal(data, &t) != nil {
		return Tag{}, tagCorrupt
	}
	return t, tagOK
}

// ReleaseAllMine walks the project base and reaps every tag whose agent_id
// matches agentID. It is best-effort: per-file errors are collected but do
// not abort the walk. Returns a summary of what was released.
func (l *LOTO) ReleaseAllMine(agentID string) (released []string, errs []error) {
	filesDir := filepath.Join(l.baseDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, []error{&ErrSystem{Op: "release-all-mine: read files dir", Err: err}}
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tag") {
			continue
		}
		tagPath := filepath.Join(filesDir, e.Name())
		lockPath := strings.TrimSuffix(tagPath, ".tag") + ".lock"
		if target, ok, err := reapTagIfMine(lockPath, tagPath, agentID); err != nil {
			errs = append(errs, err)
		} else if ok {
			released = append(released, target)
		}
	}

	globalLockPath, globalTagPath := l.globalPaths()
	if _, ok, err := reapTagIfMine(globalLockPath, globalTagPath, agentID); err != nil {
		errs = append(errs, err)
	} else if ok {
		released = append(released, "global")
	}

	// Reservations: advisory glob holds belonging to this agent. Cleared on
	// session stop so a crashed agent's reservations don't linger past its
	// lifetime (bead loto-df8).
	resPatterns, resErrs := l.unreserveAllMine(agentID)
	for _, p := range resPatterns {
		released = append(released, "reservation:"+p)
	}
	errs = append(errs, resErrs...)

	return released, errs
}

// unreserveAllMine removes every reservation file whose agent_id matches.
// Returns the patterns released. Best-effort: per-file errors are collected.
//
// Multi-pass: a concurrent Reserve from another agent can rewrite our tag
// between our ReadDir and per-file read, causing us to skip a file we own.
// We re-walk while progress is still being made, capped at maxPasses.
// Surfaced via the stress gauntlet (bead loto-8ru).
func (l *LOTO) unreserveAllMine(agentID string) (patterns []string, errs []error) {
	const maxPasses = 4
	resDir := l.reservationsDir()
	for range maxPasses {
		removed, passErrs, done := unreserveAllMinePass(l, resDir, agentID)
		patterns = append(patterns, removed...)
		errs = append(errs, passErrs...)
		if done || len(removed) == 0 {
			return patterns, errs
		}
	}
	return patterns, errs
}

// unreserveAllMinePass walks resDir once, removes any reservation file whose
// agent_id matches. done=true means the directory does not exist (terminal).
func unreserveAllMinePass(l *LOTO, resDir, agentID string) (removed []string, errs []error, done bool) {
	entries, err := os.ReadDir(resDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, true
		}
		return nil, []error{&ErrSystem{Op: "release-all-mine: read reservations dir", Err: err}}, true
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) != reservationExt {
			continue
		}
		tagPath := filepath.Join(resDir, e.Name())
		r, readErr := l.readReservation(tagPath)
		if readErr != nil {
			continue
		}
		if r == nil || r.AgentID != agentID {
			continue
		}
		if err := os.Remove(tagPath); err != nil && !os.IsNotExist(err) {
			errs = append(errs, &ErrSystem{Op: "release-all-mine: remove reservation", Err: err})
			continue
		}
		removed = append(removed, r.Pattern)
	}
	return removed, errs, false
}

// reapTagIfMine reads tagPath, and if the tag belongs to agentID, releases
// the lock+tag pair. Returns the tag's target on success.
// (target, true, nil) = released; (_, false, nil) = not ours / unreadable; (_, _, err) = reap failed.
func reapTagIfMine(lockPath, tagPath, agentID string) (string, bool, error) {
	tag, status := loadTag(tagPath)
	if status != tagOK || tag.AgentID != agentID {
		return "", false, nil
	}
	if err := reapLockFile(lockPath, tagPath); err != nil {
		return "", false, err
	}
	return tag.Target, true, nil
}

// reapLockFile attempts to remove a tag by acquiring the lock exclusively.
// Only succeeds if the flock is currently free (holder has exited).
//
// Brief retry under contention: concurrent acquires from other agents take
// the same flock momentarily during lazy-GC checks, causing benign
// EWOULDBLOCK that should not stop our cleanup. (bead loto-8ru)
func reapLockFile(lockPath, tagPath string) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := flockExclusiveWithRetry(f, 200*time.Millisecond); err != nil {
		return err
	}
	_ = os.Remove(tagPath)
	return nil
}

// ReleasePath releases an acquire'd record-tier hold on target by agentID.
// Idempotent: returns nil if no tag exists (per bead, hooks may bypass
// pre-write and post-write must not error). Returns *ErrNotMine if the
// existing tag belongs to a different agent — the call refuses to release
// (no silent steal).
func (l *LOTO) ReleasePath(agentID, target string) error {
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return err
	}

	tag, status := loadTag(fileTagPath)
	switch status {
	case tagMissing:
		return nil // idempotent silent success
	case tagCorrupt:
		// Treat corrupt as not-mine; refuse rather than wipe blindly.
		return &ErrNotMine{Target: target}
	case tagOK:
		// fall through to ownership check below.
	}
	if tag.AgentID != agentID {
		t := tag
		return &ErrNotMine{Tag: &t, Target: target}
	}

	// Same agent — take flock briefly then remove. Brief retry under
	// contention since concurrent acquire/release on the same tag is a
	// benign race (both reach for the same flock for a microsecond).
	// Surfaced via stress (bead loto-8ru).
	f, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return &ErrSystem{Op: "release-path: open file lock", Err: err}
	}
	defer f.Close()
	if err := flockExclusiveWithRetry(f, 200*time.Millisecond); err != nil {
		return &ErrSystem{Op: "release-path: flock", Err: err}
	}
	if err := os.Remove(fileTagPath); err != nil && !os.IsNotExist(err) {
		return &ErrSystem{Op: "release-path: remove tag", Err: err}
	}
	return nil
}
