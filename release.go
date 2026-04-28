package loto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// ReleaseAllMine walks the project base and reaps every tag whose agent_id
// matches agentID. It is best-effort: per-file errors are collected but do
// not abort the walk. Returns a summary of what was released.
func (l *LOTO) ReleaseAllMine(agentID string) (released []string, errs []error) {
	filesDir := filepath.Join(l.baseDir, "files")
	entries, err := os.ReadDir(filesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, []error{err}
	}

	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tag") {
			continue
		}
		tagPath := filepath.Join(filesDir, e.Name())
		data, err := os.ReadFile(tagPath)
		if err != nil {
			continue
		}
		var tag Tag
		if json.Unmarshal(data, &tag) != nil {
			continue
		}
		if tag.AgentID != agentID {
			continue
		}
		// Derive the lock path from the tag path.
		lockPath := strings.TrimSuffix(tagPath, ".tag") + ".lock"
		if err := reapLockFile(lockPath, tagPath); err != nil {
			errs = append(errs, err)
		} else {
			released = append(released, tag.Target)
		}
	}

	// Also check the global tag.
	_, globalTagPath := l.globalPaths()
	data, err := os.ReadFile(globalTagPath)
	if err == nil {
		var tag Tag
		if json.Unmarshal(data, &tag) == nil && tag.AgentID == agentID {
			globalLockPath, _ := l.globalPaths()
			if err := reapLockFile(globalLockPath, globalTagPath); err != nil {
				errs = append(errs, err)
			} else {
				released = append(released, "global")
			}
		}
	}

	return released, errs
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
