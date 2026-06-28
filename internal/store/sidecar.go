package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

// CC writes a per-process session sidecar at ~/.claude/sessions/<pid>.json.
// The schema observed in CC 2.1.x:
//   {"pid":N,"sessionId":"…","cwd":"…","startedAt":…,"procStart":"…",
//    "version":"…","peerProtocol":1,"kind":"interactive","entrypoint":"cli",
//    "status":"busy|idle|…","updatedAt":…}
// Only pid and cwd are load-bearing for doctor's zombie cross-check.

const (
	SidecarReasonNoSidecar   = "no-cc-sidecar"
	SidecarReasonCwdMismatch = "cc-cwd-mismatch"
)

type ccSidecar struct {
	PID int    `json:"pid"`
	CWD string `json:"cwd"`
}

// SidecarFinding annotates a held lock with a stronger zombie signal derived
// from the CC session sidecar. Empty SidecarFindings is the healthy state.
type SidecarFinding struct {
	PID    int
	Target string
	Reason string
	Detail string
}

// DefaultSidecarDir returns ~/.claude/sessions. Empty string if HOME is unset.
func DefaultSidecarDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".claude", "sessions")
}

// readSidecar loads ~/.claude/sessions/<pid>.json. Returns (nil, os.ErrNotExist)
// when the file is missing — callers treat that as the no-sidecar signal.
func readSidecar(dir string, pid int) (*ccSidecar, error) {
	if dir == "" {
		return nil, os.ErrNotExist
	}
	path := filepath.Join(dir, strconv.Itoa(pid)+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sc ccSidecar
	if err := json.Unmarshal(b, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}

// sidecarMissing reports true only when the file is absent. Permission errors
// and other read failures are not treated as "missing" — they're indeterminate.
func sidecarMissing(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
