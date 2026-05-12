package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var ErrAgentNotFound = errors.New("no such agent on this host")

type Agent struct {
	UUID      string    `json:"uuid"`
	Handle    string    `json:"handle"`
	CreatedAt time.Time `json:"created_at"`
	Host      string    `json:"host"`
}

func registryDir() string {
	return filepath.Join(os.Getenv("HOME"), ".loto", "agents")
}

func sessionDir() string {
	return filepath.Join(os.Getenv("HOME"), ".loto", "session")
}

var (
	fallbackWarnOnce    sync.Once
	errNoSessionCache   = errors.New("no session cache")
	errInvalidSessionID = errors.New("invalid session id")
)

// sessionCachePath validates sid (must not contain path separators or '..')
// and returns the absolute path of its cache file. Rejecting traversal here
// keeps callers tight and silences gosec G304/G703.
func sessionCachePath(sid string) (string, error) {
	if sid == "" || strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		return "", errInvalidSessionID
	}
	return filepath.Join(sessionDir(), sid+".json"), nil
}

// Ensure returns the current session's agent. If LOTO_AGENT_ID is set and
// resolves, returns it. Otherwise creates a new identity, writes it.
func Ensure() (*Agent, error) {
	u, set := os.LookupEnv("LOTO_AGENT_ID")
	if set && u != "" {
		if a, err := loadByUUID(u); err == nil {
			return a, nil
		}
	}
	// Empty-but-set LOTO_AGENT_ID means "force new agent" (test fixtures use
	// this to spin distinct identities). Unset means "interactive shell, no
	// hook" — fall back to the most recent agent on this host so lock/unlock
	// can pair across invocations.
	if !set {
		if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
			return ensureForSession(sid)
		}
		fallbackWarnOnce.Do(func() {
			fmt.Fprintln(os.Stderr, "⚠ loto: CLAUDE_CODE_SESSION_ID unset — using mostRecentAgent fallback; identity may not be stable across concurrent sessions")
		})
		if a, err := mostRecentAgent(); err == nil && a != nil {
			return a, nil
		}
	}
	return newAgent()
}

// ensureForSession resolves a stable identity for one Claude Code session
// via ~/.loto/session/<sid>.json. Mints + caches on first call.
func ensureForSession(sid string) (*Agent, error) {
	if a, err := loadSessionAgent(sid); err == nil {
		return a, nil
	}
	a, err := newAgent()
	if err != nil {
		return nil, err
	}
	_ = saveSessionAgent(sid, a)
	return a, nil
}

// loadSessionAgent reads ~/.loto/session/<sid>.json and resolves the cached
// agent UUID. Returns errNoSessionCache when no usable cache exists.
func loadSessionAgent(sid string) (*Agent, error) {
	path, err := sessionCachePath(sid)
	if err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var ref struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(body, &ref); err != nil {
		return nil, err
	}
	if ref.UUID == "" {
		return nil, errNoSessionCache
	}
	a, err := loadByUUID(ref.UUID)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return nil, errNoSessionCache
	}
	return a, nil
}

// saveSessionAgent caches the agent UUID for a Claude Code session via the
// same temp+rename pattern as writeAgent — concurrent readers see either
// the previous mapping or the new one, never partial JSON.
func saveSessionAgent(sid string, a *Agent) error {
	final, err := sessionCachePath(sid)
	if err != nil {
		return err
	}
	dir := sessionDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body, err := json.Marshal(struct {
		UUID      string    `json:"uuid"`
		CachedAt  time.Time `json:"cached_at"`
		SessionID string    `json:"session_id"`
	}{a.UUID, time.Now().UTC(), sid})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, sid+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, final) //nolint:gosec // final is validated by sessionCachePath
}

func mostRecentAgent() (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	var best *Agent
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(registryDir(), e.Name()))
		if err != nil {
			continue
		}
		var a Agent
		if err := json.Unmarshal(body, &a); err != nil {
			continue
		}
		if a.Host != host {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			cp := a
			best = &cp
		}
	}
	return best, nil
}

func newAgent() (*Agent, error) {
	if err := os.MkdirAll(registryDir(), 0o700); err != nil {
		return nil, err
	}
	uuid := newUUID()
	handle := randomHandle()
	host, _ := os.Hostname()
	a := &Agent{UUID: uuid, Handle: handle, CreatedAt: time.Now().UTC(), Host: host}
	if err := writeAgent(a); err != nil {
		return nil, err
	}
	return a, nil
}

func loadByUUID(uuid string) (*Agent, error) {
	path := filepath.Join(registryDir(), uuid+".json")
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Agent
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

func resolveByHandle(handle string) (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(registryDir(), e.Name()))
		if err != nil {
			continue
		}
		var a Agent
		if err := json.Unmarshal(body, &a); err != nil {
			continue
		}
		if a.Handle == handle {
			return &a, nil
		}
	}
	return nil, fmt.Errorf("%w: %q", ErrAgentNotFound, handle)
}

// Resolve accepts either a uuid or a handle.
func Resolve(s string) (*Agent, error) {
	if a, err := loadByUUID(s); err == nil {
		return a, nil
	}
	return resolveByHandle(s)
}

func writeAgent(a *Agent) error {
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	dir := registryDir()
	final := filepath.Join(dir, a.UUID+".json")
	// Atomic publish: write to a sibling temp, fsync, rename over the final
	// path. Concurrent readers see either the previous version or the new
	// one — never a truncated/partial JSON document (gh#50 / loto-200).
	tmp, err := os.CreateTemp(dir, a.UUID+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, final)
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}
