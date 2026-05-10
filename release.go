package loto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

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

	return released, errs
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
func reapLockFile(lockPath, tagPath string) error {
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
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

	// Same agent — take flock briefly then remove.
	f, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return &ErrSystem{Op: "release-path: open file lock", Err: err}
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
		// flock held — a foreground holder under the same agentID is using
		// the file right now. Don't yank the tag from under them.
		if isFlockContention(err) {
			return &ErrSystem{Op: "release-path: flock", Err: err}
		}
		return &ErrSystem{Op: "release-path: flock", Err: err}
	}
	if err := os.Remove(fileTagPath); err != nil && !os.IsNotExist(err) {
		return &ErrSystem{Op: "release-path: remove tag", Err: err}
	}
	return nil
}
