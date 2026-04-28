// Package loto provides lock-out / tag-out coordination for agents
// editing files in a shared workspace.
//
// Two scopes:
//
//   - File lock: exclusive on a single normalized target path.
//   - Global lock: exclusive across the workspace; blocks while any
//     file lock is held, and is blocked by any held file lock.
//
// Each acquired lock writes a JSON "tag" describing the holder so other
// agents can report blockage clearly. Locks use flock(2) and are advisory.
// This is a single-host coordination tool; NFS / network shares are not
// supported (flock semantics on networked filesystems are unreliable).
//
// Race notes:
//
//   - On acquire we Flock, then write the tag. Observers that ReadTag
//     during this gap see no tag but will still fail to acquire (the
//     flock is the source of truth). Tags are descriptive metadata.
//   - On release we Remove the tag, then drop the flock. So a brief
//     window exists where the tag is gone but the lock is technically
//     still held; clients who probed only the tag would say "free"
//     while a TryLock would still succeed a moment later. This is
//     acceptable: flock is authoritative.
package loto

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Tag describes the holder of a lock.
type Tag struct {
	AgentID   string    `json:"agent_id"`
	Intent    string    `json:"intent"`
	Target    string    `json:"target"`
	Kind      string    `json:"kind"` // "file" or "global"
	Host      string    `json:"host,omitempty"`
	PID       int       `json:"pid"`
	Branch    string    `json:"branch,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// LOTO coordinates locks under a shared base directory.
type LOTO struct {
	baseDir string
}

// ActiveLock represents an acquired lock. Call Unlock to release.
// The zero value is not usable; obtain one from a Try* method.
type ActiveLock struct {
	globalFile    *os.File // shared (file lock) or exclusive (global lock)
	fileFile      *os.File // exclusive, only set for file locks
	globalTagPath string   // set only for a global lock
	fileTagPath   string   // set only for a file lock
}

// New creates a LOTO at baseDir, ensuring the directory layout exists.
func New(baseDir string) (*LOTO, error) {
	if err := os.MkdirAll(filepath.Join(baseDir, "files"), 0o700); err != nil {
		return nil, fmt.Errorf("loto: create base dir: %w", err)
	}
	return &LOTO{baseDir: baseDir}, nil
}

// TryFileLock non-blockingly acquires a shared global lock and an
// exclusive lock on target. On failure, callers may use ReadTag /
// ReadGlobalTag to discover the blocker.
func (l *LOTO) TryFileLock(agentID, intent, target string) (*ActiveLock, error) {
	globalLockPath, _ := l.globalPaths()
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return nil, err
	}

	globalFile, err := os.OpenFile(globalLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("loto: open global lock: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = globalFile.Close()
		}
	}()

	if err := flockShared(globalFile); err != nil {
		if tag, tagErr := l.ReadGlobalTag(); tagErr == nil {
			return nil, fmt.Errorf("loto: global lock held by %s (%s)", tag.AgentID, tag.Intent)
		}
		return nil, fmt.Errorf("loto: global lock held: %w", err)
	}

	fileFile, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("loto: open file lock: %w", err)
	}
	defer func() {
		if !success {
			_ = fileFile.Close()
		}
	}()

	if err := flockExclusive(fileFile); err != nil {
		if tag, tagErr := l.ReadTag(target); tagErr == nil {
			return nil, fmt.Errorf("loto: %s held by %s (%s)", target, tag.AgentID, tag.Intent)
		}
		return nil, fmt.Errorf("loto: %s held: %w", target, err)
	}

	tag := l.newTag(agentID, intent, target, "file")
	if err := writeTagAtomic(fileTagPath, tag); err != nil {
		return nil, err
	}

	success = true
	return &ActiveLock{
		globalFile:  globalFile,
		fileFile:    fileFile,
		fileTagPath: fileTagPath,
	}, nil
}

// TryGlobalLock non-blockingly acquires an exclusive global lock.
// This fails while any file lock is active.
func (l *LOTO) TryGlobalLock(agentID, intent string) (*ActiveLock, error) {
	globalLockPath, globalTagPath := l.globalPaths()

	globalFile, err := os.OpenFile(globalLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("loto: open global lock: %w", err)
	}
	success := false
	defer func() {
		if !success {
			_ = globalFile.Close()
		}
	}()

	if err := flockExclusive(globalFile); err != nil {
		if tag, tagErr := l.ReadGlobalTag(); tagErr == nil {
			return nil, fmt.Errorf("loto: global lock held by %s (%s)", tag.AgentID, tag.Intent)
		}
		return nil, fmt.Errorf("loto: global lock held: %w", err)
	}

	tag := l.newTag(agentID, intent, "global", "global")
	if err := writeTagAtomic(globalTagPath, tag); err != nil {
		return nil, err
	}

	success = true
	return &ActiveLock{
		globalFile:    globalFile,
		globalTagPath: globalTagPath,
	}, nil
}

// Unlock removes tags then releases flocks. Calling Unlock more than once
// is safe; subsequent calls are no-ops. Returns the first error
// encountered.
func (al *ActiveLock) Unlock() error {
	var firstErr error
	note := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if al.fileTagPath != "" {
		if err := os.Remove(al.fileTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			note(err)
		}
		al.fileTagPath = ""
	}
	if al.globalTagPath != "" {
		if err := os.Remove(al.globalTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			note(err)
		}
		al.globalTagPath = ""
	}
	if al.fileFile != nil {
		// Closing the fd releases the flock.
		note(al.fileFile.Close())
		al.fileFile = nil
	}
	if al.globalFile != nil {
		note(al.globalFile.Close())
		al.globalFile = nil
	}
	return firstErr
}

// ReadTag returns the tag for target's file lock, if any. Tags may be
// stale if the writer crashed; treat as advisory.
func (l *LOTO) ReadTag(target string) (*Tag, error) {
	_, tagPath, err := l.filePaths(target)
	if err != nil {
		return nil, err
	}
	return readTag(tagPath)
}

// ReadGlobalTag returns the tag for the global lock, if any.
func (l *LOTO) ReadGlobalTag() (*Tag, error) {
	_, tagPath := l.globalPaths()
	return readTag(tagPath)
}

// Break administratively force-releases a file lock by attempting to take
// it exclusively (which only succeeds when no live process holds it),
// then removing the tag. Returns an error if the lock is currently held.
func (l *LOTO) Break(target string) error {
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("loto: open file lock: %w", err)
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
		return fmt.Errorf("loto: %s is currently held; refusing to break", target)
	}
	if err := os.Remove(fileTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (l *LOTO) globalPaths() (lockPath, tagPath string) {
	return filepath.Join(l.baseDir, "global.lock"),
		filepath.Join(l.baseDir, "global.tag")
}

func (l *LOTO) filePaths(target string) (lockPath, tagPath string, err error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", "", fmt.Errorf("loto: normalize target %q: %w", target, err)
	}
	name := hashTarget(filepath.Clean(abs))
	return filepath.Join(l.baseDir, "files", name+".lock"),
		filepath.Join(l.baseDir, "files", name+".tag"),
		nil
}

func hashTarget(target string) string {
	sum := sha256.Sum256([]byte(target))
	return hex.EncodeToString(sum[:])
}

func (l *LOTO) newTag(agentID, intent, target, kind string) Tag {
	host, _ := os.Hostname() // best effort
	cwd, _ := os.Getwd()     // best effort
	return Tag{
		AgentID:   agentID,
		Intent:    intent,
		Target:    target,
		Kind:      kind,
		Host:      host,
		PID:       os.Getpid(),
		Branch:    gitBranch(cwd),
		Cwd:       cwd,
		Timestamp: time.Now().UTC(),
	}
}

func writeTagAtomic(tagPath string, tag Tag) error {
	data, err := json.Marshal(tag)
	if err != nil {
		return fmt.Errorf("loto: marshal tag: %w", err)
	}
	tmpPath := tagPath + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("loto: create tmp tag: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("loto: write tmp tag: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("loto: sync tmp tag: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("loto: close tmp tag: %w", err)
	}
	if err := os.Rename(tmpPath, tagPath); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("loto: rename tmp tag: %w", err)
	}
	return nil
}

func readTag(tagPath string) (*Tag, error) {
	data, err := os.ReadFile(tagPath)
	if err != nil {
		return nil, err
	}
	var tag Tag
	if err := json.Unmarshal(data, &tag); err != nil {
		return nil, fmt.Errorf("loto: parse tag: %w", err)
	}
	return &tag, nil
}

func gitBranch(dir string) string {
	cmd := exec.Command("git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
