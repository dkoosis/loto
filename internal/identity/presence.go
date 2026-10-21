package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Sessions returns every loto session record on this host, sorted by session
// id. Raw, like readSession: no pruning, no verdict. An unreadable session
// dir returns nil — no records is what `loto check --worktree` then reads
// from Claude Code's own records, which is where an absent loto record turns
// into "unknown" rather than "free" (loto-nwoz).
func Sessions() []SessionRecord {
	entries, err := os.ReadDir(sessionDir())
	if err != nil {
		return nil
	}
	out := make([]SessionRecord, 0, len(entries))
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if rec, ok := readSession(strings.TrimSuffix(e.Name(), ".json")); ok {
			out = append(out, rec)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out
}

// IsCaller reports whether this record is the calling session's own: its
// session id, or an earlier id of the same process (/clear rotates the id and
// leaves the process up, loto-2jgn). Unknown process identity never matches.
func (r SessionRecord) IsCaller() bool {
	if sid := SessionIDFromEnv(); sid != "" && r.SessionID == sid {
		return true
	}
	return r.sameProcess(callerProc())
}

// ClaudeSession is Claude Code's own record of one running session, at
// ~/.claude/sessions/<pid>.json. Claude Code writes it for every session,
// whether or not loto's SessionStart hook ever ran — which is why it is the
// witness for a session loto has no record of. Only the fields presence
// needs are decoded.
type ClaudeSession struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	Socket    string `json:"messagingSocketPath"`
}

// ClaudeSessions reads every Claude Code session record on this host, sorted
// by pid. A record that does not parse, or names no pid, is skipped: it
// carries nothing to probe. A missing directory is no sessions.
func ClaudeSessions() []ClaudeSession {
	dir := filepath.Join(homeDir(), ".claude", "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []ClaudeSession
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var c ClaudeSession
		if json.Unmarshal(body, &c) != nil || c.PID <= 0 {
			continue
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

// Liveness judges a Claude Code record by its pid, and by its messaging
// socket when it names one: a recorded socket that is gone, or a pid not
// running, is dead; otherwise live.
//
// ‡ No start-time check. Claude Code records its start as a locale clock
// string ("Wed Oct 21 19:14:10 2026") that no reader here produces, so a
// recycled pid reads live. That errs toward "occupied", which costs a caller
// one kept worktree; the other direction costs a session its tree.
func (c ClaudeSession) Liveness() SessionLiveness {
	if c.Socket != "" {
		if _, err := os.Stat(c.Socket); err != nil {
			return SessionDead
		}
	}
	if !PIDAlive(c.PID) {
		return SessionDead
	}
	return SessionLive
}

// IsCaller reports whether this record is the calling session's own, by
// session id or by the session pid the caller's env names.
func (c ClaudeSession) IsCaller() bool {
	if sid := SessionIDFromEnv(); sid != "" && c.SessionID == sid {
		return true
	}
	me := callerProc()
	return me.pid > 0 && c.PID == me.pid
}
