package identity

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// handleShape constrains LOTO_HANDLE input to the PascalCase adjective+noun
// form randomHandle emits. The hyphen in the second group accommodates noun
// list entries like "aye-aye" and "musk-ox".
var handleShape = regexp.MustCompile(`^[A-Z][a-z]+(?:[A-Z][a-z-]+)+$`)

var errInvalidHandle = errors.New("invalid LOTO_HANDLE")

// agentsGCMaxAge bounds how long an unused agent record may linger in
// ~/.loto/agents/ before Ensure prunes it. Anything older than this is
// overwhelmingly likely to be dead (crashed session, ephemeral pre-fix run).
const agentsGCMaxAge = 30 * 24 * time.Hour

var agentsGCOnce sync.Once

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
	staleWarnOnce       sync.Once
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
// resolves, returns it. Empty-but-set LOTO_AGENT_ID requests an ephemeral
// in-memory identity — fleet dispatchers export this to keep the registry
// from accumulating one orphan .json per subagent. Unset falls back through
// session cache then mostRecentAgent for interactive shells.
func Ensure() (*Agent, error) {
	agentsGCOnce.Do(func() { _ = gcStaleAgents(time.Now()) })

	u, set := os.LookupEnv("LOTO_AGENT_ID")
	if set {
		if u != "" {
			if a, err := loadByUUID(u); err == nil {
				return a, nil
			}
			// Stale LOTO_AGENT_ID — set but not resolvable. Falling through to
			// ephemeral mints a fresh in-memory uuid on every call, so any
			// locks acquired in this session can't be released (the second
			// invocation sees a different uuid). Warn loudly (audit loto-16t).
			staleWarnOnce.Do(func() {
				fmt.Fprintf(os.Stderr, "✗ loto: LOTO_AGENT_ID=%s is set but no agent record exists; using ephemeral identity (locks acquired here cannot be released by other invocations)\n", u)
			})
		}
		return mintAgent()
	}
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		return ensureForSession(sid)
	}
	fallbackWarnOnce.Do(func() {
		fmt.Fprintln(os.Stderr, "✗ loto: CLAUDE_CODE_SESSION_ID unset — using mostRecentAgent fallback; identity may not be stable across concurrent sessions")
	})
	if a, err := mostRecentAgent(); err == nil && a != nil {
		return a, nil
	}
	return newAgent()
}

// ensureForSession resolves a stable identity for one Claude Code session
// via ~/.loto/session/<sid>.json. Mints + caches on first call. The cache
// file is created with O_CREATE|O_EXCL so concurrent first-use callers
// (e.g. SessionStart hook + an immediate `loto inbox`) converge on one
// identity; the loser drops its candidate agent file and adopts the
// winner's record (gh#28).
func ensureForSession(sid string) (*Agent, error) {
	if a, err := loadSessionAgent(sid); err == nil {
		return a, nil
	}
	candidate, err := newAgent()
	if err != nil {
		return nil, err
	}
	err = claimSessionCache(sid, candidate)
	if err == nil {
		return candidate, nil
	}
	if !errors.Is(err, fs.ErrExist) {
		return nil, err
	}
	_ = os.Remove(filepath.Join(registryDir(), candidate.UUID+".json"))
	// Loser raced the winner between O_EXCL create and Write+Close. ReadFile
	// can observe a 0-byte file in that window; retry briefly so the loser
	// sees the winner's published mapping. If the winner crashed mid-write,
	// the retries fail fast (~100ms total) and the caller surfaces the error.
	var lastErr error
	for range 20 {
		a, lerr := loadSessionAgent(sid)
		if lerr == nil {
			return a, nil
		}
		lastErr = lerr
		time.Sleep(5 * time.Millisecond)
	}
	return nil, lastErr
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

// claimSessionCache attempts to create ~/.loto/session/<sid>.json with
// O_CREATE|O_EXCL — first writer wins the sid mapping; the loser receives
// fs.ErrExist and re-reads via loadSessionAgent (gh#28).
func claimSessionCache(sid string, a *Agent) error {
	final, err := sessionCachePath(sid)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
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
	f, err := os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, werr := f.Write(body); werr != nil {
		f.Close()
		_ = os.Remove(final)
		return werr
	}
	if serr := f.Sync(); serr != nil {
		f.Close()
		_ = os.Remove(final)
		return serr
	}
	return f.Close()
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
	a, err := mintAgent()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(registryDir(), 0o700); err != nil {
		return nil, err
	}
	if err := writeAgent(a); err != nil {
		return nil, err
	}
	return a, nil
}

func mintAgent() (*Agent, error) {
	handle, err := chooseHandle()
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	return &Agent{UUID: newUUID(), Handle: handle, CreatedAt: time.Now().UTC(), Host: host}, nil
}

func chooseHandle() (string, error) {
	if h, ok := os.LookupEnv("LOTO_HANDLE"); ok && h != "" {
		if !handleShape.MatchString(h) {
			return "", fmt.Errorf("%w: %q (want PascalCase adjective+noun, e.g. SwiftFalcon)", errInvalidHandle, h)
		}
		return h, nil
	}
	return randomHandle(), nil
}

// gcStaleAgents removes ~/.loto/agents/*.json whose mtime is older than
// agentsGCMaxAge. Best-effort: errors are swallowed (a missing dir, a denied
// unlink, a racing writer) — staleness is a hygiene concern, not a hard
// invariant. Stops scanning the moment ReadDir fails.
func gcStaleAgents(now time.Time) error {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return err
	}
	cutoff := now.Add(-agentsGCMaxAge)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			_ = os.Remove(filepath.Join(registryDir(), e.Name()))
		}
	}
	return nil
}

// LookupByUUID returns the agent record for uuid, or an error if no record
// exists on disk. Used by render to print holder Handle alongside UUID in
// conflict reports (loto-b3o).
func LookupByUUID(uuid string) (*Agent, error) {
	return loadByUUID(uuid)
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

func writeAgent(a *Agent) error {
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	dir := registryDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
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

// NewUUID returns a fresh RFC 4122 v4 UUID. Exported so non-identity callers
// (e.g. CLI runtime session-id minting) can use the same generator without
// duplicating the bit-twiddling.
func NewUUID() string { return newUUID() }

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
