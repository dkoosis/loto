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

// handleShape constrains LOTO_HANDLE to the same general PascalCase display
// shape as generated handles. It does not require membership in the built-in
// adjective/animal lists. The hyphen in the second group accommodates entries
// like "aye-aye" and "musk-ox".
var handleShape = regexp.MustCompile(`^[A-Z][a-z]+(?:[A-Z][a-z-]+)+$`)

// agentIDShape matches the canonical UUID hex layout. It is not a strict
// RFC 4122 v4 check — version/variant bits aren't enforced — because its
// job is to block path traversal before filepath.Join, not to police
// UUID provenance. newUUID always emits v4, so values produced by this
// tool satisfy both shapes regardless.
var agentIDShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

var (
	errInvalidHandle  = errors.New("invalid LOTO_HANDLE")
	errInvalidAgentID = errors.New("invalid LOTO_AGENT_ID")
	errStaleAgentID   = errors.New("stale LOTO_AGENT_ID")
)

// fallbackFreshness bounds how recently mostRecentAgent's pick must have
// been created to be usable as an interactive fallback. Older records are
// almost certainly from a long-finished session; reusing them would silently
// re-attribute new locks to a dead identity.
const fallbackFreshness = 24 * time.Hour

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

// Ensure resolves the current agent identity by the contract documented in
// the package doc. The governing principle: identity ambiguity is allowed
// for display, never for authority — an explicit but unresolvable
// LOTO_AGENT_ID is a hard error, and the heuristic mostRecentAgent fallback
// is only consulted when fresh.
func Ensure() (*Agent, error) {
	agentsGCOnce.Do(func() { _ = gcStaleAgents(time.Now()) })

	u, set := os.LookupEnv("LOTO_AGENT_ID")
	if set {
		if u == "" {
			return mintAgent()
		}
		if !agentIDShape.MatchString(u) {
			return nil, fmt.Errorf("%w: %q (want canonical uuid hex form)", errInvalidAgentID, u)
		}
		a, err := loadByUUID(u)
		if err == nil {
			return a, nil
		}
		// An explicit uuid that doesn't resolve is the caller asserting an
		// identity that does not exist. Silently substituting an ephemeral
		// identity (the pre-loto-16t behavior) orphans every lock acquired
		// in the session, since the next invocation sees a different uuid.
		// Fail loud instead.
		return nil, fmt.Errorf("%w: %q (no agent record at %s)", errStaleAgentID, u, filepath.Join(registryDir(), u+".json"))
	}
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		return ensureForSession(sid)
	}
	if a, err := mostRecentAgent(time.Now()); err == nil && a != nil {
		fallbackWarnOnce.Do(func() {
			fmt.Fprintf(os.Stderr, "✗ loto: CLAUDE_CODE_SESSION_ID unset — reusing recent agent %s (%s); set CLAUDE_CODE_SESSION_ID or LOTO_AGENT_ID for stable identity\n", a.Handle, a.UUID)
		})
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
//
// Ordering is load-bearing: newAgent writes the candidate's agent file
// before claimSessionCache publishes the session→uuid mapping — referent
// before reference. A loser that reads the winner's cache is therefore
// guaranteed to find the winner's agent file already on disk; flipping
// to a mint→claim→write-on-win ordering would widen the retry loop
// below (currently scoped to the 0-byte cache window) to also absorb
// agent-write latency. The os.Remove cleanup runs only on the ErrExist
// branch; a non-ErrExist claim failure orphans the candidate agent
// file, but gcStaleAgents reaps it at 30 days, which is the right
// amount of cleanup at this scale.
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
	if lastErr == nil {
		lastErr = errNoSessionCache
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
	return loadByUUID(ref.UUID)
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

// mostRecentAgent returns the newest local agent created within
// fallbackFreshness of now, or nil if no such record exists. Stale entries
// are deliberately excluded — a 30-day-old record represents a long-finished
// session, and reusing it would silently re-attribute new locks to a dead
// identity.
func mostRecentAgent(now time.Time) (*Agent, error) {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return nil, err
	}
	host, _ := os.Hostname()
	cutoff := now.Add(-fallbackFreshness)
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
		if a.CreatedAt.Before(cutoff) {
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
// agentsGCMaxAge, except any uuid still referenced by a session cache file
// in ~/.loto/session/. Preserving referenced uuids keeps the binding
// invariant: as long as a session cache exists, the agent it points to
// resolves. Best-effort otherwise: errors are swallowed (missing dir,
// denied unlink, racing writer) — staleness is hygiene, not invariant.
func gcStaleAgents(now time.Time) error {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return err
	}
	pinned := sessionReferencedUUIDs()
	cutoff := now.Add(-agentsGCMaxAge)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		uuid := strings.TrimSuffix(e.Name(), ".json")
		if _, keep := pinned[uuid]; keep {
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

// sessionReferencedUUIDs returns the set of agent uuids that any session
// cache currently points at. Used by gcStaleAgents to avoid breaking a
// session→agent binding from underneath a live session.
func sessionReferencedUUIDs() map[string]struct{} {
	out := map[string]struct{}{}
	entries, err := os.ReadDir(sessionDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(sessionDir(), e.Name()))
		if err != nil {
			continue
		}
		var ref struct {
			UUID string `json:"uuid"`
		}
		if err := json.Unmarshal(body, &ref); err != nil {
			continue
		}
		if ref.UUID != "" {
			out[ref.UUID] = struct{}{}
		}
	}
	return out
}

// LookupByUUID returns the agent record for uuid, or an error if no record
// exists on disk. Used by render to print holder Handle alongside UUID in
// conflict reports (loto-b3o).
func LookupByUUID(uuid string) (*Agent, error) {
	return loadByUUID(uuid)
}

func loadByUUID(uuid string) (*Agent, error) {
	if !agentIDShape.MatchString(uuid) {
		return nil, fmt.Errorf("%w: %q", errInvalidAgentID, uuid)
	}
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
	// crypto/rand.Read on Linux/macOS is backed by getrandom(2) / arc4random;
	// a failure here means the kernel CSPRNG is unavailable, which is a
	// program-environment failure, not a user error. Panic rather than
	// emit a zeroed (and thus colliding) "uuid".
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Errorf("identity: crypto/rand unavailable: %w", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]), hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]), hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}
