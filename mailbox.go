package loto

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	mailboxMaxAge    = 30 * 24 * time.Hour // messages older than this are dropped on read
	mailboxCompactAt = 200                 // compact after this many appends
)

// Msg is a single mailbox message.
type Msg struct {
	From      string    `json:"from"`
	To        string    `json:"to"` // agent handle/id, or "@all"
	Body      string    `json:"body"`
	Timestamp time.Time `json:"timestamp"`
	System    bool      `json:"system,omitempty"` // true for loto-generated notices
}

// SendMsg appends a message to the mailbox for target.
// Returns an error only on IO failure; message delivery is best-effort.
func (l *LOTO) SendMsg(target, from, to, body string, system bool) error {
	_, _, err := l.filePaths(target)
	if err != nil {
		return &ErrSystem{Op: "msg: resolve paths", Err: err}
	}
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return err
	}
	msg := Msg{
		From:      from,
		To:        to,
		Body:      body,
		Timestamp: time.Now().UTC(),
		System:    system,
	}
	return appendMsg(msgsPath, msg)
}

// ReadMsgs returns all non-expired messages for target, filtered to those
// addressed to agentID or @all. Messages older than mailboxMaxAge are dropped.
func (l *LOTO) ReadMsgs(target, agentID string) ([]Msg, error) {
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return nil, err
	}
	all, err := readMsgs(msgsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &ErrSystem{Op: "msg: read mailbox", Err: err}
	}
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
	return out, nil
}

// CompactMsgs rewrites the mailbox dropping messages older than mailboxMaxAge.
func (l *LOTO) CompactMsgs(target string) error {
	msgsPath, err := l.msgsPath(target)
	if err != nil {
		return err
	}
	all, err := readMsgs(msgsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return &ErrSystem{Op: "msg: compact read", Err: err}
	}
	cutoff := time.Now().Add(-mailboxMaxAge)
	var keep []Msg
	for _, m := range all {
		if !m.Timestamp.Before(cutoff) {
			keep = append(keep, m)
		}
	}
	return rewriteMsgs(msgsPath, keep)
}

func (l *LOTO) msgsPath(target string) (string, error) {
	_, tagPath, err := l.filePaths(target)
	if err != nil {
		return "", &ErrSystem{Op: "msg: resolve paths", Err: err}
	}
	return strings.TrimSuffix(tagPath, ".tag") + ".msgs", nil
}

func appendMsg(msgsPath string, msg Msg) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return &ErrSystem{Op: "msg: marshal", Err: err}
	}
	f, err := os.OpenFile(msgsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return &ErrSystem{Op: "msg: open mailbox", Err: err}
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return &ErrSystem{Op: "msg: write mailbox", Err: err}
	}

	// Compact opportunistically after reaching the threshold.
	if info, err := f.Stat(); err == nil {
		lines := countLines(msgsPath)
		if lines >= mailboxCompactAt {
			// Best-effort; ignore error.
			_ = compactFile(msgsPath)
		}
		_ = info
	}
	return nil
}

func readMsgs(msgsPath string) ([]Msg, error) {
	f, err := os.Open(msgsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var msgs []Msg
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var m Msg
		if json.Unmarshal([]byte(line), &m) == nil {
			msgs = append(msgs, m)
		}
	}
	return msgs, scanner.Err()
}

func rewriteMsgs(msgsPath string, msgs []Msg) error {
	tmp := msgsPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return &ErrSystem{Op: "msg: compact create", Err: err}
	}
	for _, m := range msgs {
		data, _ := json.Marshal(m)
		if _, err := f.Write(append(data, '\n')); err != nil {
			f.Close()
			_ = os.Remove(tmp)
			return &ErrSystem{Op: "msg: compact write", Err: err}
		}
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return &ErrSystem{Op: "msg: compact close", Err: err}
	}
	if err := os.Rename(tmp, msgsPath); err != nil {
		_ = os.Remove(tmp)
		return &ErrSystem{Op: "msg: compact rename", Err: err}
	}
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
	msgs, err := readMsgs(msgsPath)
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-mailboxMaxAge)
	var keep []Msg
	for _, m := range msgs {
		if !m.Timestamp.Before(cutoff) {
			keep = append(keep, m)
		}
	}
	return rewriteMsgs(msgsPath, keep)
}

// msgPath returns the .msgs path for a hash (used by doctor/GC).
func (l *LOTO) msgPathForHash(hash string) string {
	return filepath.Join(l.baseDir, "files", hash+".msgs")
}
