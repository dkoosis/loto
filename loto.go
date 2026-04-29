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
	"context"
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

// ErrHeld is returned when a lock is currently held by another agent.
// Use errors.As to extract the holder's Tag and Kind.
//
//nolint:errname // sentinel-style name kept for API stability
type ErrHeld struct {
	Tag    *Tag   // advisory; may be nil if the tag was unreadable
	Kind   string // "file" or "global"
	Target string // the requested target path or "global"
}

func (e *ErrHeld) Error() string {
	if e.Tag != nil {
		return fmt.Sprintf("loto: %s held by %s (%s)", e.Target, e.Tag.AgentID, e.Tag.Intent)
	}
	return fmt.Sprintf("loto: %s is held", e.Target)
}

// MarshalJSON emits the NS holder-report shape so CLI can write structured
// blocker JSON directly to stderr without reformatting.
func (e *ErrHeld) MarshalJSON() ([]byte, error) {
	type report struct {
		BlockedBy string `json:"blocked_by,omitempty"`
		Intent    string `json:"intent,omitempty"`
		Kind      string `json:"kind"`
		Target    string `json:"target"`
		HeldSince string `json:"held_since,omitempty"`
		ExpiresAt string `json:"expires_at,omitempty"`
		Branch    string `json:"branch,omitempty"`
		Host      string `json:"host,omitempty"`
		PID       int    `json:"pid,omitempty"`
	}
	r := report{Kind: e.Kind, Target: e.Target}
	if e.Tag != nil {
		r.BlockedBy = e.Tag.AgentID
		r.Intent = e.Tag.Intent
		r.Branch = e.Tag.Branch
		r.Host = e.Tag.Host
		r.PID = e.Tag.PID
		if !e.Tag.Timestamp.IsZero() {
			r.HeldSince = e.Tag.Timestamp.Format(time.RFC3339)
		}
	}
	return json.Marshal(r)
}

// ErrSystem wraps an IO or OS-level failure (exit code 3 at the CLI).
//
//nolint:errname // sentinel-style name kept for API stability
type ErrSystem struct {
	Op  string
	Err error
}

func (e *ErrSystem) Error() string { return fmt.Sprintf("loto: %s: %v", e.Op, e.Err) }
func (e *ErrSystem) Unwrap() error { return e.Err }

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
	ExpiresAt time.Time `json:"expires_at,omitempty"` // zero = no TTL
}

// SoftStale reports whether the tag's advisory TTL has expired.
// A soft-stale tag may still hold the flock (flock remains authoritative).
func (t *Tag) SoftStale() bool {
	return !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt)
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

	// Conflicts lists advisory reservations that overlap the locked target.
	// Non-nil only when reservations exist that match the path; the lock is
	// still granted (reservations are advisory).
	Conflicts []*Reservation
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
// Pass a tagOptions with TTL to set an advisory expiry on the tag.
func (l *LOTO) TryFileLock(agentID, intent, target string, opts ...TagOptions) (*ActiveLock, error) {
	globalLockPath, _ := l.globalPaths()
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return nil, err
	}

	globalFile, err := os.OpenFile(globalLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &ErrSystem{Op: "open global lock", Err: err}
	}
	success := false
	defer func() {
		if !success {
			_ = globalFile.Close()
		}
	}()

	if err := flockShared(globalFile); err != nil {
		tag, _ := l.ReadGlobalTag()
		return nil, &ErrHeld{Tag: tag, Kind: "global", Target: "global"}
	}

	fileFile, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &ErrSystem{Op: "open file lock", Err: err}
	}
	defer func() {
		if !success {
			_ = fileFile.Close()
		}
	}()

	if err := flockExclusive(fileFile); err != nil {
		tag, _ := l.ReadTag(target)
		return nil, &ErrHeld{Tag: tag, Kind: "file", Target: target}
	}

	// Lazy-GC: flock succeeded but a stale tag from a dead process remains.
	lazyReapTag(fileTagPath)

	tag := l.newTag(agentID, intent, target, "file", opts...)
	if err := writeTagAtomic(fileTagPath, tag); err != nil {
		return nil, &ErrSystem{Op: "write tag", Err: err}
	}

	success = true
	lock := &ActiveLock{
		globalFile:  globalFile,
		fileFile:    fileFile,
		fileTagPath: fileTagPath,
	}
	// Advisory: attach any conflicting reservations so callers can warn.
	lock.Conflicts, _ = l.ConflictingReservations(target)
	return lock, nil
}

// TryGlobalLock non-blockingly acquires an exclusive global lock.
// This fails while any file lock is active.
func (l *LOTO) TryGlobalLock(agentID, intent string, opts ...TagOptions) (*ActiveLock, error) {
	globalLockPath, globalTagPath := l.globalPaths()

	globalFile, err := os.OpenFile(globalLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, &ErrSystem{Op: "open global lock", Err: err}
	}
	success := false
	defer func() {
		if !success {
			_ = globalFile.Close()
		}
	}()

	if err := flockExclusive(globalFile); err != nil {
		tag, _ := l.ReadGlobalTag()
		return nil, &ErrHeld{Tag: tag, Kind: "global", Target: "global"}
	}

	lazyReapTag(globalTagPath)

	tag := l.newTag(agentID, intent, "global", "global", opts...)
	if err := writeTagAtomic(globalTagPath, tag); err != nil {
		return nil, &ErrSystem{Op: "write tag", Err: err}
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

// ReapIfDead reaps target's file lock tag only if the tag's recorded PID is no
// longer alive. Returns nil if the tag was reaped or was already absent.
// Returns ErrHeld if the process is still running.
func (l *LOTO) ReapIfDead(target string) error {
	tag, err := l.ReadTag(target)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return &ErrSystem{Op: "reap-if-dead: read tag", Err: err}
	}
	if pidAlive(tag.PID) {
		return &ErrHeld{Tag: tag, Kind: "file", Target: target}
	}
	return l.Reap(target)
}

// lazyReapTag silently removes a tag file if its recorded PID is dead.
// Called after a successful flock acquire to clean up crash leftovers.
func lazyReapTag(tagPath string) {
	data, err := os.ReadFile(tagPath)
	if err != nil {
		return
	}
	var tag Tag
	if json.Unmarshal(data, &tag) != nil {
		return
	}
	if !pidAlive(tag.PID) {
		_ = os.Remove(tagPath)
	}
}

// Reap removes a stale tag on target when no live process holds the lock.
// It succeeds only if the flock is currently acquirable (i.e., the previous
// holder has exited). Returns ErrHeld if the lock is still live.
//
// Reap is safe cleanup, not forced takeover. For a forced-takeover that
// notifies the displaced agent, see the planned ForceBreak (loto-7wp.19).
func (l *LOTO) Reap(target string) error {
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return &ErrSystem{Op: "reap: resolve paths", Err: err}
	}
	f, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return &ErrSystem{Op: "reap: open file lock", Err: err}
	}
	defer f.Close()
	if err := flockExclusive(f); err != nil {
		tag, _ := l.ReadTag(target)
		return &ErrHeld{Tag: tag, Kind: "file", Target: target}
	}
	if err := os.Remove(fileTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &ErrSystem{Op: "reap: remove tag", Err: err}
	}
	return nil
}

// ForceBreak administratively takes a currently-held file lock, notifying
// the displaced agent via their mailbox. Unlike Reap, it succeeds even when
// the lock is live — it takes the flock by closing the holder's descriptor
// (kernel reclaims on process death) or by waiting if the holder is still up.
//
// byAgent is the agent performing the break; reason is logged in the mailbox
// notice. Mailbox delivery is best-effort; ForceBreak returns nil as long as
// the lock itself was cleared.
func (l *LOTO) ForceBreak(target, byAgent, reason string) error {
	fileLockPath, fileTagPath, err := l.filePaths(target)
	if err != nil {
		return &ErrSystem{Op: "force-break: resolve paths", Err: err}
	}

	// Read tag before acquiring — we need the displaced holder's identity for
	// the mailbox notice. Tag may be nil if holder crashed before writing it.
	displaced, _ := l.ReadTag(target)

	f, err := os.OpenFile(fileLockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return &ErrSystem{Op: "force-break: open file lock", Err: err}
	}
	defer f.Close()

	// flockExclusive blocks until the current holder releases (process exit
	// or explicit unlock). This is intentional for ForceBreak — the caller
	// accepts waiting.
	if err := flockExclusiveBlocking(f); err != nil {
		return &ErrSystem{Op: "force-break: acquire flock", Err: err}
	}

	// Notify displaced agent (best-effort).
	if displaced != nil {
		body := fmt.Sprintf("lock on %q force-broken by %s: %s", target, byAgent, reason)
		_ = l.SendMsg(target, byAgent, displaced.AgentID, body, true)
	}

	if err := os.Remove(fileTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &ErrSystem{Op: "force-break: remove tag", Err: err}
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

// TagOptions carries optional parameters for tag creation.
type TagOptions struct {
	TTL time.Duration // 0 = no expiry (advisory only; flock remains authoritative)
}

func (l *LOTO) newTag(agentID, intent, target, kind string, opts ...TagOptions) Tag {
	host, _ := os.Hostname()
	cwd, _ := os.Getwd()
	now := time.Now().UTC()
	tag := Tag{
		AgentID:   agentID,
		Intent:    intent,
		Target:    target,
		Kind:      kind,
		Host:      host,
		PID:       os.Getpid(),
		Branch:    gitBranch(cwd),
		Cwd:       cwd,
		Timestamp: now,
	}
	if len(opts) > 0 && opts[0].TTL > 0 {
		tag.ExpiresAt = now.Add(opts[0].TTL)
	}
	return tag
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
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
