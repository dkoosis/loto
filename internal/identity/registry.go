package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
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

// homeDir returns the user's home directory, preferring os.UserHomeDir ($HOME)
// but falling back to os/user.Current().HomeDir (getpwuid_r) when $HOME is
// unset. Without this fallback, an empty $HOME yields relative ".loto/agents"
// paths whose meaning changes with cwd — fragmenting identity across
// directories (gh#112 / loto-3axo).
func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	return "/tmp" // both lookups failed; /tmp keeps paths absolute
}

func registryDir() string {
	return filepath.Join(homeDir(), ".loto", "agents")
}

func sessionDir() string {
	return filepath.Join(homeDir(), ".loto", "session")
}

var (
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
func Ensure(ctx context.Context) (*Agent, error) {
	// GC runs out of band via GCAgents (driven by the CLI runtime after the
	// store is open, so it can pass the set of lock-owner UUIDs to pin).
	// Firing it here unconditionally with a nil pin set would race the
	// runtime's pin-aware call: agentsGCOnce only fires once, so whichever
	// path runs first defines the GC behavior, and Ensure() runs first.
	// That ordering was the root cause of gh#125 (loto-ffg) — stale-by-time
	// agents pinned by live locks were reaped before the runtime could
	// register them as pinned. Leave GC scheduling to GCAgents.

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
		return ensureForSession(ctx, sid)
	}
	// No explicit env binding. Per identity v4 invariant ("ambiguity allowed
	// for display, never for authority"), do not adopt any pre-existing local
	// agent — the heuristic 24h fallback could resurrect a dead session's
	// UUID and silently re-attribute new locks to it. Mint a fresh identity
	// instead; the caller can pin it by exporting LOTO_AGENT_ID.
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
func ensureForSession(ctx context.Context, sid string) (*Agent, error) {
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
	return awaitOrRecoverSession(ctx, sid)
}

// awaitOrRecoverSession retries loading the session agent (the winner may
// still be writing), then attempts crash recovery if the cache is corrupt.
func awaitOrRecoverSession(ctx context.Context, sid string) (*Agent, error) {
	for range 20 {
		a, lerr := loadSessionAgent(sid)
		if lerr == nil {
			return a, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}

	if recoverCorruptSessionCache(sid) {
		recovery, rerr := newAgent()
		if rerr != nil {
			return nil, rerr
		}
		if cerr := claimSessionCache(sid, recovery); cerr == nil {
			return recovery, nil
		}
		_ = os.Remove(filepath.Join(registryDir(), recovery.UUID+".json"))
		if a, lerr := loadSessionAgent(sid); lerr == nil {
			return a, nil
		}
	}
	return nil, errNoSessionCache
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

// recoverCorruptSessionCache checks whether the session cache file for sid
// is corrupt (0-byte or unparseable JSON) and, if so, unlinks it so the
// caller can re-claim. Returns true if the file was removed (or already
// absent), false if the file exists and is valid (another writer repaired
// it concurrently). This is the recovery path for gh#115: a winner that
// crashed between O_EXCL Create and Write/Sync leaves a permanently broken
// cache file; without recovery every future caller returns errNoSessionCache.
func recoverCorruptSessionCache(sid string) bool {
	path, err := sessionCachePath(sid)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		// File gone (another racer cleaned up) — caller can re-claim.
		return !errors.Is(err, fs.ErrPermission)
	}
	// Non-empty, parseable, with a uuid → the file is valid; no recovery.
	var ref struct {
		UUID string `json:"uuid"`
	}
	if len(body) > 0 && json.Unmarshal(body, &ref) == nil && ref.UUID != "" {
		return false
	}
	// Corrupt: 0-byte, bad JSON, or empty uuid. Unlink and let caller retry.
	_ = os.Remove(path)
	return true
}

// claimSessionCache attempts to create ~/.loto/session/<sid>.json with
// O_CREATE|O_EXCL — first writer wins the sid mapping; the loser receives
// fs.ErrExist and re-reads via loadSessionAgent (gh#28).
func claimSessionCache(sid string, a *Agent) error {
	final, err := sessionCachePath(sid)
	if err != nil {
		return err
	}
	if err := mkdirAllSync(sessionDir()); err != nil {
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
	if cerr := f.Close(); cerr != nil {
		_ = os.Remove(final)
		return cerr
	}
	// Durably record the new directory entry (loto-cq6 / gh#131). Best-effort:
	// the O_EXCL claim has already won and the file's bytes are fsync'd, so a
	// dir-flush IO error must not retract a valid claim — the caller treats any
	// non-ErrExist error as fatal (see ensureForSession). Next invocation
	// re-reads the file that exists.
	_ = syncDir(sessionDir())
	return nil
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
// in ~/.loto/session/ OR present in extraPinned. extraPinned carries
// owner_uuids drawn from live lock rows (see GCAgents): pruning an agent
// pinned by a live lock would strand the lock with an unresolvable owner,
// so LookupByUUID(holder) returns ENOENT for an active holder. Identity
// package cannot import store directly without a dependency cycle, so the
// caller passes the pin set in. Preserving referenced uuids keeps the
// binding invariant: as long as a session cache or live lock points at
// an agent, it resolves. Best-effort otherwise: errors are swallowed
// (missing dir, denied unlink, racing writer) — staleness is hygiene,
// not invariant. Regression for gh#125 (loto-ffg).
func gcStaleAgents(now time.Time, extraPinned map[string]struct{}) error {
	entries, err := os.ReadDir(registryDir())
	if err != nil {
		return err
	}
	pinned := sessionReferencedUUIDs()
	for u := range extraPinned {
		pinned[u] = struct{}{}
	}
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

// GCAgents is the public entry point for the agent-registry GC pass. The
// CLI runtime calls it after opening the store, passing the set of
// owner_uuids drawn from live lock rows. This is the path that closes the
// gh#125 race where the once-per-process GC in Ensure() runs before the
// store is open and so reaps agents still pinned by active locks. Wraps
// gcStaleAgents; runs at most once per process via agentsGCOnce so
// Ensure()'s best-effort call and this call don't double up.
func GCAgents(now time.Time, lockOwnerUUIDs map[string]struct{}) error {
	var err error
	agentsGCOnce.Do(func() { err = gcStaleAgents(now, lockOwnerUUIDs) })
	return err
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
	if err := mkdirAllSync(dir); err != nil {
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
	if err := os.Rename(tmpName, final); err != nil {
		return err
	}
	// An fsync'd file's directory entry is not itself durable until the
	// containing dir is fsync'd — power loss between the rename and the dir
	// metadata flush can lose the new name (loto-cq6 / gh#131).
	return syncDir(dir)
}

// syncDir flushes a directory's metadata to stable storage so that a rename
// or O_EXCL create performed inside it survives power loss. Call after the
// file itself has been fsync'd. (Duplicated in internal/cli rather than shared
// via a helper package: identity must import no internal package — see
// .go-arch-lint.yml. The helper is small enough to fall under jscpd limits.)
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return err
	}
	return d.Close()
}

// mkdirAllSync is os.MkdirAll(dir, 0o700) plus a parent-dir fsync when dir was
// newly created, so the new directory entry survives power loss (loto-4n65,
// same durability class as loto-cq6). A pre-existing directory is a no-op — no
// extra fsync. Only the immediate parent is flushed; loto's ~/.loto parent
// already exists, so MkdirAll never creates more than one level here. A path
// that exists as a non-directory falls through to MkdirAll, which surfaces the
// real "not a directory" error rather than being masked. 0o700 is fixed: every
// identity dir under ~/.loto is user-private.
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dir))
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
