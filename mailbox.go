package loto

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	mailboxMaxAge    = 30 * 24 * time.Hour // messages older than this are dropped on read
	mailboxCompactAt = 200                 // compact after this many appends
)

// Importance values for Msg.Importance. Empty == normal.
const (
	ImportanceLow    = "low"
	ImportanceNormal = "normal"
	ImportanceUrgent = "urgent"
)

// Msg is a single mailbox message.
//
// MsgID is a UUIDv4 set on first append; it makes appends idempotent (a retry
// with the same ID is dropped) and lets the compactor dedupe by identity
// rather than by line. Legacy messages without MsgID are passed through
// unchanged.
//
// ThreadID is an optional caller-supplied conversation key (e.g. a bead ID).
// loto neither generates nor interprets it; it exists so consumers can group
// related messages without reusing Body.
//
// AckRequired signals the sender wants confirmation the recipient saw this
// (e.g. a "please release" note to a lock holder). loto stamps ReadAt on the
// recipient's first ReadMsgs call for direct messages — senders read the
// mailbox to check it. Importance is an advisory hint (low|normal|urgent);
// loto stores but does not enforce delivery semantics from it.
type Msg struct {
	MsgID       string     `json:"msg_id,omitempty"`
	ThreadID    string     `json:"thread_id,omitempty"`
	From        string     `json:"from"`
	To          string     `json:"to"` // agent handle/id, or "@all"
	Body        string     `json:"body"`
	Timestamp   time.Time  `json:"timestamp"`
	System      bool       `json:"system,omitempty"` // true for loto-generated notices
	AckRequired bool       `json:"ack_required,omitempty"`
	ReadAt      *time.Time `json:"read_at,omitempty"` // stamped on recipient's first read of a direct message
	Importance  string     `json:"importance,omitempty"`
}

// newMsgID returns a random UUIDv4 in 8-4-4-4-12 hex form. Falls back to a
// timestamp-derived hex string if the system entropy source fails — uniqueness
// is degraded but the dedupe path still functions.
func newMsgID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Last-resort fallback: nanosecond timestamp padded to 16 bytes.
		ns := uint64(time.Now().UnixNano())
		for i := range b {
			b[i] = byte((ns >> (8 * (i % 8))) & 0xff)
		}
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// SendMsg appends a message to the mailbox for target. A fresh MsgID is
// generated; for idempotent retries (same ID across calls), use SendMsgWith.
// Returns an error only on IO failure; message delivery is best-effort.
func (l *LOTO) SendMsg(target, from, to, body string, system bool) error {
	return l.SendMsgWith(target, Msg{
		From:   from,
		To:     to,
		Body:   body,
		System: system,
	})
}

// SendMsgWith appends msg, filling in MsgID and Timestamp if empty. If a
// message with the same non-empty MsgID is already in the mailbox, the call
// is a no-op — making retries safe to repeat.
func (l *LOTO) SendMsgWith(target string, msg Msg) error {
	if _, _, err := l.filePaths(target); err != nil {
		return &ErrSystem{Op: "msg: resolve paths", Err: err}
	}
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return err
	}
	if msg.MsgID == "" {
		msg.MsgID = newMsgID()
	}
	if msg.Timestamp.IsZero() {
		msg.Timestamp = time.Now().UTC()
	}
	return appendMsg(msgsPath, msg)
}

// ReadMsgs returns all non-expired messages for target, filtered to those
// addressed to agentID or @all. Messages older than mailboxMaxAge are dropped.
//
// As a side effect, ReadMsgs stamps Msg.ReadAt = now for any direct message
// (To == agentID, not @all) whose ReadAt is unset, then rewrites the mailbox
// under the per-mailbox lock. Senders read the file later to check ACK. @all
// messages are never stamped — they have no single recipient.
func (l *LOTO) ReadMsgs(target, agentID string) ([]Msg, error) {
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return nil, err
	}
	if _, statErr := os.Stat(msgsPath); os.IsNotExist(statErr) {
		return nil, nil
	}
	var out []Msg
	if err := withMailboxLock(msgsPath, func() error {
		all, rerr := readMsgs(msgsPath)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return nil
			}
			return &ErrSystem{Op: "msg: read mailbox", Err: rerr}
		}
		stamped := stampReadAt(all, agentID)
		out = filterVisible(all, agentID)
		if stamped {
			return rewriteMsgs(msgsPath, all)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// stampReadAt sets ReadAt = now on every direct message in all addressed to
// agentID whose ReadAt is unset. Returns true if any stamp was applied.
func stampReadAt(all []Msg, agentID string) bool {
	if agentID == "" || agentID == "@all" {
		return false
	}
	now := time.Now().UTC()
	stamped := false
	for i := range all {
		if all[i].To == agentID && all[i].ReadAt == nil {
			t := now
			all[i].ReadAt = &t
			stamped = true
		}
	}
	return stamped
}

// filterVisible returns messages addressed to agentID or @all, dropping any
// older than mailboxMaxAge.
func filterVisible(all []Msg, agentID string) []Msg {
	cutoff := time.Now().Add(-mailboxMaxAge)
	var out []Msg
	for _, m := range all {
		if m.Timestamp.Before(cutoff) {
			continue
		}
		if m.To != "@all" && m.To != agentID {
			continue
		}
		out = append(out, m)
	}
	return out
}

// CompactMsgs rewrites the mailbox dropping messages older than mailboxMaxAge.
func (l *LOTO) CompactMsgs(target string) error {
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return err
	}
	return compactFile(msgsPath)
}

// withMailboxLock serializes append/compact for a single mailbox by taking
// a blocking exclusive flock on a sidecar lock file. This closes the
// append-vs-compact race where O_APPEND writes to the original inode were
// lost when compactFile's rename swapped in a snapshot taken pre-write.
func withMailboxLock(msgsPath string, fn func() error) error {
	lockPath := msgsPath + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return &ErrSystem{Op: "msg: open mailbox lock", Err: err}
	}
	defer func() { _ = lf.Close() }()
	if err := flockExclusiveBlocking(lf); err != nil {
		return &ErrSystem{Op: "msg: acquire mailbox lock", Err: err}
	}
	defer func() { _ = flockRelease(lf) }()
	return fn()
}

func (l *LOTO) msgsPath(target string) (string, error) {
	_, tagPath, err := l.filePaths(target)
	if err != nil {
		return "", &ErrSystem{Op: "msg: resolve paths", Err: err}
	}
	return strings.TrimSuffix(tagPath, ".tag") + ".msgs", nil
}

func appendMsg(msgsPath string, msg Msg) error {
	return withMailboxLock(msgsPath, func() error {
		return appendMsgLocked(msgsPath, msg)
	})
}

// appendMsgLocked writes msg to msgsPath; caller must hold the mailbox lock.
// Data is fsynced before close, and the parent directory is fsynced after the
// first creation, so an acknowledged append survives a crash/power loss.
//
// If msg.MsgID is set and already present in the file, the call is a no-op —
// retries are safe regardless of how many times they fire.
func appendMsgLocked(msgsPath string, msg Msg) error {
	if msg.MsgID != "" {
		dup, err := mailboxContainsID(msgsPath, msg.MsgID)
		if err != nil {
			return err
		}
		if dup {
			return nil
		}
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return &ErrSystem{Op: "msg: marshal", Err: err}
	}
	_, statErr := os.Stat(msgsPath)
	creating := os.IsNotExist(statErr)
	f, err := os.OpenFile(msgsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return &ErrSystem{Op: "msg: open mailbox", Err: err}
	}
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return &ErrSystem{Op: "msg: write mailbox", Err: err}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return &ErrSystem{Op: "msg: fsync mailbox", Err: err}
	}
	if err := f.Close(); err != nil {
		return &ErrSystem{Op: "msg: close mailbox", Err: err}
	}
	if creating {
		// Newly created file: parent dir must be fsynced or the entry itself
		// may not survive reboot on some filesystems.
		_ = fsyncDir(filepath.Dir(msgsPath))
	}

	if countLines(msgsPath) >= mailboxCompactAt {
		_ = compactFileLocked(msgsPath)
	}
	return nil
}

// fsyncDir flushes a directory's metadata so renames/creates within it are
// durable across a crash. Best-effort: some filesystems don't support it.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// mailboxContainsID reports whether msgsPath already contains a message with
// the given non-empty MsgID. Returns false (no error) if the file does not
// exist. Reads line-by-line and unmarshals only the msg_id field for speed.
func mailboxContainsID(msgsPath, id string) (bool, error) {
	f, err := os.Open(msgsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, &ErrSystem{Op: "msg: dedupe scan", Err: err}
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	var probe struct {
		MsgID string `json:"msg_id"`
	}
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue // corrupt lines are handled by readMsgs/quarantine path
		}
		if probe.MsgID == id {
			return true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return false, &ErrSystem{Op: "msg: dedupe scan", Err: err}
	}
	return false, nil
}

func readMsgs(msgsPath string) ([]Msg, error) {
	f, err := os.Open(msgsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var msgs []Msg
	var corrupt []string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var m Msg
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			corrupt = append(corrupt, line)
			continue
		}
		msgs = append(msgs, m)
	}
	if err := scanner.Err(); err != nil {
		return msgs, err
	}
	if len(corrupt) > 0 {
		quarantineCorruptLines(msgsPath, corrupt)
	}
	return msgs, nil
}

// quarantineCorruptLines appends corrupt mailbox lines to a sidecar file and
// emits a warning. Best-effort: failures are logged but do not propagate, so
// corruption visibility never breaks the read path.
func quarantineCorruptLines(msgsPath string, lines []string) {
	corruptPath := msgsPath + ".corrupt"
	fmt.Fprintf(os.Stderr, "loto: warning: %d corrupt line(s) in mailbox %s; quarantined to %s\n",
		len(lines), msgsPath, corruptPath)
	f, err := os.OpenFile(corruptPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loto: warning: cannot open quarantine file %s: %v\n", corruptPath, err)
		return
	}
	defer f.Close()
	stamp := time.Now().UTC().Format(time.RFC3339Nano)
	for _, line := range lines {
		fmt.Fprintf(f, "%s\t%s\n", stamp, line)
	}
}

// rewriteMsgs atomically replaces msgsPath with msgs. The temp file is
// fsynced before rename, and the parent directory is fsynced after, so a
// crash anywhere along the way leaves either the old contents or the new —
// never a truncated file or a missing rename.
func rewriteMsgs(msgsPath string, msgs []Msg) error {
	tmp := msgsPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return &ErrSystem{Op: "msg: compact create", Err: err}
	}
	for _, m := range msgs {
		data, err := json.Marshal(m)
		if err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return &ErrSystem{Op: "msg: compact marshal", Err: err}
		}
		if _, err := f.Write(append(data, '\n')); err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return &ErrSystem{Op: "msg: compact write", Err: err}
		}
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return &ErrSystem{Op: "msg: compact fsync", Err: err}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return &ErrSystem{Op: "msg: compact close", Err: err}
	}
	if err := os.Rename(tmp, msgsPath); err != nil {
		_ = os.Remove(tmp)
		return &ErrSystem{Op: "msg: compact rename", Err: err}
	}
	_ = fsyncDir(filepath.Dir(msgsPath))
	return nil
}

func countLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) != "" {
			n++
		}
	}
	return n
}

func compactFile(msgsPath string) error {
	return withMailboxLock(msgsPath, func() error {
		return compactFileLocked(msgsPath)
	})
}

// compactFileLocked drops expired messages and rewrites msgsPath; caller
// must hold the mailbox lock.
func compactFileLocked(msgsPath string) error {
	msgs, err := readMsgs(msgsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	cutoff := time.Now().Add(-mailboxMaxAge)
	keep := make([]Msg, 0, len(msgs))
	// Dedupe by MsgID, keeping the first occurrence (preserves arrival order).
	// Legacy messages without MsgID are never deduped — they pass through.
	seen := make(map[string]struct{}, len(msgs))
	for _, m := range msgs {
		if m.Timestamp.Before(cutoff) {
			continue
		}
		if m.MsgID != "" {
			if _, dup := seen[m.MsgID]; dup {
				continue
			}
			seen[m.MsgID] = struct{}{}
		}
		keep = append(keep, m)
	}
	return rewriteMsgs(msgsPath, keep)
}
