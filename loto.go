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

// Repeated kind/target string literals consolidated for goconst.
const (
	kindGlobal = "global"
	kindFile   = "file"
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

// ErrNotMine is returned by ReleasePath when the caller is not the holder
// of the named target. Distinct from ErrHeld so callers and the CLI can
// route it to a different exit code/message ("you don't own this lock").
//
//nolint:errname // sentinel-style name kept for API stability
type ErrNotMine struct {
	Tag    *Tag   // holder's tag (descriptive)
	Target string // the requested target path
}

func (e *ErrNotMine) Error() string {
	if e.Tag != nil {
		return fmt.Sprintf("loto: %s held by %s, not by caller — refusing to release", e.Target, e.Tag.AgentID)
	}
	return fmt.Sprintf("loto: %s not held by caller", e.Target)
}

// MarshalJSON emits a structured shape suitable for CLI stderr.
func (e *ErrNotMine) MarshalJSON() ([]byte, error) {
	type report struct {
		HeldBy string `json:"held_by,omitempty"`
		Intent string `json:"intent,omitempty"`
		Target string `json:"target"`
		PID    int    `json:"pid,omitempty"`
	}
	r := report{Target: e.Target}
	if e.Tag != nil {
		r.HeldBy = e.Tag.AgentID
		r.Intent = e.Tag.Intent
		r.PID = e.Tag.PID
	}
	return json.Marshal(r)
}

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
	ExpiresAt time.Time `json:"expires_at,omitzero"` // zero = no TTL
}

// SoftStale reports whether the tag's advisory TTL has expired.
// A soft-stale tag may still hold the flock (flock remains authoritative).
func (t *Tag) SoftStale() bool {
	return !t.ExpiresAt.IsZero() && time.Now().After(t.ExpiresAt)
}

// IsRecordTier reports whether the tag represents a record-tier acquire'd
// hold whose authority is currently in force. True iff the tag carries a
// non-zero ExpiresAt that is still in the future — see north-star
// "Tags are descriptive, flock is authoritative — with one bounded
// exception" carve-out.
func (t *Tag) IsRecordTier() bool {
	return !t.ExpiresAt.IsZero() && !t.SoftStale()
}

// LOTO coordinates locks under a shared base directory.
type LOTO struct {
	baseDir string

	// ZombieIdle is the maximum interval doctor allows between observed
	// activity (tag timestamp, mailbox writes, target mtime) and "now"
	// before flagging a held lock as a zombie. Zero = use default.
	ZombieIdle time.Duration
}

// ActiveLock represents an acquired lock. Call Unlock to release.
// The zero value is not usable; obtain one from a Try* method.
type ActiveLock struct {
	globalFile    *os.File // shared (file lock) or exclusive (global lock)
	fileFile      *os.File // exclusive, only set for file locks
	globalTagPath string   // set only for a global lock
	fileTagPath   string   // set only for a file lock

	// preserveTag is set when TryFileLock layered atop an existing record-tier
	// tag belonging to this same agent. Unlock then releases the flock without
	// removing the tag — the caller's foreground use must not destroy the
	// underlying acquire'd record-tier authority. (bead loto-c4f / gh-31)
	preserveTag bool

	// Conflicts lists advisory reservations that overlap the locked target.
	// Non-nil only when reservations exist that match the path; the lock is
	// still granted (reservations are advisory).
	Conflicts []*Reservation
}

// New creates a LOTO at baseDir, ensuring the directory layout exists.
func New(baseDir string) (*LOTO, error) {
	if err := os.MkdirAll(filepath.Join(baseDir, "files"), 0o700); err != nil {
		return nil, &ErrSystem{Op: "create base dir", Err: err}
	}
	return &LOTO{baseDir: baseDir}, nil
}

// flockOrHeld attempts the given flock op; on contention returns *ErrHeld
// populated from the appropriate tag, on other errors returns *ErrSystem.
func (l *LOTO) flockOrHeld(op func(*os.File) error, f *os.File, sysOp, kind, target string) error {
	if err := op(f); err != nil {
		if !isFlockContention(err) {
			return &ErrSystem{Op: sysOp, Err: err}
		}
		var tag *Tag
		if kind == kindGlobal {
			tag, _ = l.ReadGlobalTag()
		} else {
			tag, _ = l.ReadTag(target)
		}
		return &ErrHeld{Tag: tag, Kind: kind, Target: target}
	}
	return nil
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

	if err := l.flockOrHeld(flockShared, globalFile, "flock global", kindGlobal, kindGlobal); err != nil {
		return nil, err
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

	if err := l.flockOrHeld(flockExclusive, fileFile, "flock file", kindFile, target); err != nil {
		return nil, err
	}

	preserveTag, err := l.recordTierGuard(target, agentID)
	if err != nil {
		return nil, err
	}
	if !preserveTag {
		lazyReapTag(fileTagPath)
		tag := l.newTag(agentID, intent, target, kindFile, opts...)
		if err := writeTagAtomic(fileTagPath, tag); err != nil {
			return nil, err
		}
	}

	success = true
	lock := &ActiveLock{
		globalFile:  globalFile,
		fileFile:    fileFile,
		fileTagPath: fileTagPath,
		preserveTag: preserveTag,
	}
	// Advisory: attach any conflicting reservations so callers can warn.
	lock.Conflicts, _ = l.ConflictingReservations(target)
	return lock, nil
}

// recordTierGuard checks for an existing record-tier tag at target.
// Returns preserveTag=true when the same agent already holds a record-tier
// tag (the foreground try must layer on top, not overwrite). Returns
// ErrHeld when a different agent holds it. (bead loto-c4f / gh-31)
func (l *LOTO) recordTierGuard(target, agentID string) (bool, error) {
	existing, _ := l.ReadTag(target)
	if existing == nil || !existing.IsRecordTier() {
		return false, nil
	}
	if existing.AgentID != agentID {
		return false, &ErrHeld{Tag: existing, Kind: kindFile, Target: target}
	}
	return true, nil
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
		if !isFlockContention(err) {
			return nil, &ErrSystem{Op: "flock global", Err: err}
		}
		tag, _ := l.ReadGlobalTag()
		return nil, &ErrHeld{Tag: tag, Kind: kindGlobal, Target: kindGlobal}
	}

	// Global tier is process-lifetime only; TTL respect here is incidental,
	// not a contract — no current code path puts TTL on a global tag.
	lazyReapTag(globalTagPath)

	tag := l.newTag(agentID, intent, kindGlobal, kindGlobal, opts...)
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

	if al.fileTagPath != "" && !al.preserveTag {
		if err := os.Remove(al.fileTagPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			note(err)
		}
	}
	al.fileTagPath = ""
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
		return &ErrHeld{Tag: tag, Kind: kindFile, Target: target}
	}
	return l.Reap(target)
}

// lazyReapTag silently removes a tag file if its recorded PID is dead AND
// the tag does not represent a live record-tier acquire'd hold (non-zero,
// unexpired ExpiresAt). Called after a successful flock acquire to clean
// up crash leftovers without destroying valid acquire'd holds.
func lazyReapTag(tagPath string) {
	data, err := os.ReadFile(tagPath)
	if err != nil {
		return
	}
	var tag Tag
	if json.Unmarshal(data, &tag) != nil {
		return
	}
	if pidAlive(tag.PID) {
		return
	}
	// Process is dead. Spare the tag if it's a still-live record-tier hold.
	if tag.IsRecordTier() {
		return
	}
	_ = os.Remove(tagPath)
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
		if !isFlockContention(err) {
			return &ErrSystem{Op: "reap: flock", Err: err}
		}
		tag, _ := l.ReadTag(target)
		return &ErrHeld{Tag: tag, Kind: kindFile, Target: target}
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
		return "", "", &ErrSystem{Op: fmt.Sprintf("normalize target %q", target), Err: err}
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
		return &ErrSystem{Op: "marshal tag", Err: err}
	}
	tmpPath := tagPath + ".tmp"
	tmp, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return &ErrSystem{Op: "create tmp tag", Err: err}
	}
	// On any error past this point, drop the half-written temp file.
	fail := func(op string, err error) error {
		_ = os.Remove(tmpPath)
		return &ErrSystem{Op: op + " tmp tag", Err: err}
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fail("write", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close", err)
	}
	if err := os.Rename(tmpPath, tagPath); err != nil {
		return fail("rename", err)
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

// gitBranchTimeout caps the cost of stamping a tag's branch when git is slow,
// hung, or unreachable (e.g. a stale .git lock).
const gitBranchTimeout = 5 * time.Second

func gitBranch(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitBranchTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "branch", "--show-current")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
