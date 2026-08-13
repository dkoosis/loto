package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Peer is the Claude Code messaging identity of the process behind a loto
// agent: the name SendMessage addresses and the socket that proves the
// session is still up. It is deliberately a SEPARATE record from Agent —
// identity is durable and immutable, presence is volatile and self-pruning,
// so a peer record never rewrites (and never churns the mtime of) the
// 10k-file agent registry.
//
// What is actually derivable inside a hook, honestly:
//   - Socket, PID, SessionID, CWD — always, straight from env.
//   - Name — only for SUBAGENTS, whose peer name is in their own argv
//     (`--agent-name impl-ccp-3yd`). A top-level interactive session is named
//     from its cwd, or renamed by /rename, and that name appears nowhere a
//     child process can read. Such a peer records with an empty Name; `loto
//     who` prints it as unnamed and says how to fix it. LOTO_PEER_NAME (or
//     `loto whoami --peer-name`) lets a session that does know its name
//     record it.
type Peer struct {
	UUID      string    `json:"uuid"`   // agent uuid this presence belongs to
	Handle    string    `json:"handle"` // loto handle, denormalized for cheap listing
	Name      string    `json:"name,omitempty"`
	AgentID   string    `json:"agent_id,omitempty"` // CC "<name>@<team>" address, subagents only
	Socket    string    `json:"socket,omitempty"`
	PID       int       `json:"pid,omitempty"`
	SessionID string    `json:"session_id,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	SeenAt    time.Time `json:"seen_at"`
	// ParentSessionID is --parent-session-id from the recording process's argv
	// (subagents; empty for top-level sessions). Recorded so SessionVerdict can
	// detect a recycled pid: the occupant's argv must still carry the same
	// identity flags the recorder's did (loto-ygty).
	ParentSessionID string `json:"parent_session_id,omitempty"`
	// ProcStart is the OS start-time of the session process, read at RecordPeer
	// time from the same reader that will re-read it at verdict time. The value
	// is opaque and host-local (darwin: wall-clock µs; linux: ticks since boot)
	// — it is only ever compared for EQUALITY against a value read on this
	// host, never against a clock. 0 means unknown: the reader failed, the
	// platform has none, or the record predates this field. Unknown NEVER
	// escalates to dead (loto-kwlp precedent, internal/cli/runtime.go
	// pidVerdict).
	ProcStart int64 `json:"proc_start,omitempty"`
}

// Named reports whether this peer is addressable by SendMessage today.
func (p Peer) Named() bool { return p.Name != "" }

// Live reports whether the session behind this peer is still up, per the one
// liveness oracle (SessionVerdict, liveness.go). Kept as the boolean
// convenience for callers that only branch; unknown maps to true — a verdict
// this record cannot support must not read as dead.
//
// Honest residual: PID reuse is closed for any peer whose record carries a
// ProcStart (every record written by this binary on darwin or linux) — a
// recycled pid has a different OS start-time and reads DEAD. It is NOT closed
// for a record with ProcStart == 0: a record written before the field
// existed, or on a platform with no start-time reader. Such a peer falls back
// to the argv flag check, which top-level sessions do not carry, and can read
// LIVE after its pid is recycled. That population drains as records
// self-prune and sessions restart; it does not accumulate.
func (p Peer) Live(ctx context.Context) bool {
	return p.SessionVerdict(ctx).Liveness != SessionDead
}

func peersDir() string { return filepath.Join(homeDir(), ".loto", "peers") }

// tmpOrphanGrace bounds how long a *.tmp file in ~/.loto/peers may linger
// before Peers treats it as crash debris rather than an in-flight publish.
// writePeer's CreateTemp→Rename completes in milliseconds; an hour is six
// orders of magnitude of slack, so a swept file cannot plausibly be one a live
// writer is about to Rename. (Unlinking an in-flight temp would fail that
// writer's Rename with ENOENT and error its RecordPeer hook — the grace, not
// the glob, is what makes this safe.)
const tmpOrphanGrace = time.Hour

// peerPath validates uuid before joining, on the same fail-closed principle as
// sessionCachePath: a caller-supplied id must not be able to escape peersDir.
func peerPath(uuid string) (string, error) {
	if !agentIDShape.MatchString(uuid) {
		return "", fmt.Errorf("%w: %q", errInvalidAgentID, uuid)
	}
	return filepath.Join(peersDir(), uuid+".json"), nil
}

// procTimeout caps the `ps` call that reads a process's argv, so a wedged
// proc table cannot hang a hook.
const procTimeout = 2 * time.Second

// peerReadAttempts bounds readPeerFile's re-read loop. A record replaced
// inside the read→unlink window is re-judged once, so the realistic
// single-restart case reports the FRESH peer on the same `loto who`
// invocation rather than merely sparing it from the unlink. The bound
// guarantees termination: a replacement storm degrades to "print the stale
// observation, unlink nothing", never to a spin.
const peerReadAttempts = 2

// procArgv returns the full command line of pid, or "" if it cannot be read.
// Var, not func, so tests can drive the parse without a real process.
var procArgv = psArgv //nolint:gochecknoglobals // test seam for the proc-table read

// peerPruneRecheckHook, when non-nil, is invoked inside prunePeerRecord after
// the record is judged dead and before the pre-unlink re-stat. Tests use it to
// simulate a session that republishes its record inside the read→unlink TOCTOU
// window; production leaves it nil.
var peerPruneRecheckHook func()

func psArgv(ctx context.Context, pid int) string {
	if pid <= 0 {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, procTimeout)
	defer cancel()
	//nolint:gosec // G204: the only interpolated argument is strconv.Itoa of an int, and exec.Command takes an argv slice — no shell, nothing to inject
	out, err := exec.CommandContext(ctx, "ps", "-o", "command=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// argvFlag pulls "--<flag> <value>" out of a whitespace-split command line.
// Claude Code spells these as separate argv words; the "--flag=value" form is
// accepted too so a future change in spelling doesn't silently blank the name.
func argvFlag(argv, flag string) string {
	fields := strings.Fields(argv)
	for i, f := range fields {
		if v, ok := strings.CutPrefix(f, "--"+flag+"="); ok {
			return v
		}
		if f == "--"+flag && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// PeerFromEnv builds the peer record for the current process, or nil when
// this process is not running inside a Claude Code session with messaging
// enabled (no socket → nothing to address, nothing to record).
//
// name overrides the derived name when non-empty (`loto whoami --peer-name`);
// LOTO_PEER_NAME is the env spelling of the same override.
func PeerFromEnv(ctx context.Context, a *Agent, name string) *Peer {
	socket := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	if socket == "" || a == nil {
		return nil
	}
	pid := envPID(socket)
	argv := procArgv(ctx, pid)
	procStartVal, _ := ProcStart(pid) // 0,false → 0 → unknown, same as legacy
	if name == "" {
		name = os.Getenv("LOTO_PEER_NAME")
	}
	if name == "" {
		name = argvFlag(argv, "agent-name")
	}
	cwd, _ := os.Getwd()
	return &Peer{
		UUID:            a.UUID,
		Handle:          a.Handle,
		Name:            name,
		AgentID:         argvFlag(argv, "agent-id"),
		Socket:          socket,
		PID:             pid,
		SessionID:       os.Getenv("CLAUDE_CODE_SESSION_ID"),
		CWD:             cwd,
		SeenAt:          time.Now().UTC(),
		ParentSessionID: argvFlag(argv, "parent-session-id"),
		ProcStart:       procStartVal,
	}
}

// envPID resolves the session process id. CLAUDE_PID is the direct answer;
// the socket basename ("/tmp/cc-socks/63879.sock") carries the same number and
// covers a harness that stops exporting the var.
func envPID(socket string) int {
	if v, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && v > 0 {
		return v
	}
	base := strings.TrimSuffix(filepath.Base(socket), ".sock")
	if v, err := strconv.Atoi(base); err == nil && v > 0 {
		return v
	}
	return 0
}

// RecordPeer writes this process's peer record, replacing any earlier one for
// the same agent. Returns (nil, nil) outside a messaging-enabled session —
// having no peer identity is the normal case for a human shell, not an error.
func RecordPeer(ctx context.Context, a *Agent, name string) (*Peer, error) {
	p := PeerFromEnv(ctx, a, name)
	if p == nil {
		return nil, nil //nolint:nilnil // "no peer identity here" is a normal, non-error outcome
	}
	if err := writePeer(p); err != nil {
		return nil, err
	}
	return p, nil
}

func writePeer(p *Peer) error {
	path, err := peerPath(p.UUID)
	if err != nil {
		return err
	}
	body, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	dir := peersDir()
	if err := mkdirAllSync(dir); err != nil {
		return err
	}
	// Same atomic-publish discipline as writeAgent: a concurrent `loto who`
	// must never read a half-written record.
	//
	// prunePeerRecord's dev+ino guard DEPENDS on this publish-by-rename: every
	// rewrite lands a new inode at path, which is what makes os.SameFile an
	// exact replacement detector rather than a heuristic (loto-gj1z). Changing
	// this to write in place would silently reduce that guard to a no-op.
	tmp, err := os.CreateTemp(dir, p.UUID+".*.tmp")
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// readPeerRaw loads one peer record with no side effects. ok=false means
// unreadable or unparseable. The oracle (peerByUUID) reads through this: a
// liveness question must never unlink a record.
func readPeerRaw(path string) (Peer, bool) {
	p, _, ok := readPeerAt(path)
	return p, ok
}

// readPeerAt loads one peer record together with the OS identity of the file
// the bytes actually came from. Bytes and identity are taken from the SAME
// open fd, so no rewrite can slip between them: whatever a caller judges from
// these bytes, os.SameFile can later prove is or is not still at the path.
//
// Ordering matters: stat-then-read admits a skew where the identity describes
// one inode and the bytes came from another (fails safe, but silently and
// non-deterministically), and read-then-stat is actively WRONG — it would
// capture a replacement's identity and then confirm the replacement against
// itself, authorizing the exact unlink the guard exists to prevent.
func readPeerAt(path string) (Peer, os.FileInfo, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Peer{}, nil, false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return Peer{}, nil, false
	}
	body, err := io.ReadAll(f)
	if err != nil {
		return Peer{}, nil, false
	}
	var p Peer
	if err := json.Unmarshal(body, &p); err != nil {
		return Peer{}, nil, false
	}
	return p, fi, true
}

// prunePeerRecord unlinks path only if it still holds the same file the dead
// verdict was read from. writePeer publishes by CreateTemp+Rename, so every
// rewrite lands a NEW inode at the same path: dev+ino is an exact replacement
// detector here, not a heuristic — the replacement's inode cannot collide with
// the old one, because the old one is still linked when CreateTemp allocates.
// Returns false when the record was replaced (leave it alone: the new record
// was never judged) or already gone (someone else pruned it).
//
// Honest limit: this cuts the read→unlink window from seconds (a `ps` call
// bounded at procTimeout, per record, serially) to the microseconds between
// this Stat and this Remove. It does not eliminate it.
func prunePeerRecord(path string, judged os.FileInfo) bool {
	if peerPruneRecheckHook != nil {
		peerPruneRecheckHook()
	}
	now, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !os.SameFile(judged, now) {
		return false
	}
	_ = os.Remove(path)
	return true
}

// readPeerFile loads one peer record, unlinking it when the oracle says the
// session is DEAD — presence prunes itself. An UNKNOWN verdict keeps the
// record: no witness either way is not evidence of death. ok=false means "skip
// this entry": unreadable, unparseable, or dead and not asked for. An
// unreadable record is skipped rather than surfaced — a peer table is a
// convenience, and half of one still beats an error.
//
// The unlink is conditional on the file still being the one that was judged
// (loto-gj1z): a session restarting inside the seconds-wide verdict window
// republishes its record, and unlinking by path alone would delete that fresh
// record for the remaining life of the session. On detecting a replacement the
// record is re-judged, bounded by peerReadAttempts, so `loto who` reports the
// new peer on this same invocation instead of blinking.
func readPeerFile(ctx context.Context, path string, includeDead bool) (Peer, bool) {
	var p Peer
	for range peerReadAttempts {
		var (
			fi os.FileInfo
			ok bool
		)
		p, fi, ok = readPeerAt(path)
		if !ok {
			return Peer{}, false
		}
		if p.SessionVerdict(ctx).Liveness != SessionDead {
			return p, true
		}
		if prunePeerRecord(path, fi) {
			return p, includeDead
		}
		// Replaced inside the window — the record now at path was never judged,
		// so re-read and judge it on its own terms.
	}
	// Attempts exhausted against a record that keeps being replaced: report the
	// last observation, unlink nothing that was not verified.
	return p, includeDead
}

// sweepTmpOrphan removes a leftover publish temp from ~/.loto/peers once it is
// older than tmpOrphanGrace. writePeer's `defer os.Remove(tmpName)` covers
// every error path, so only a hard kill (SIGKILL, power loss) between
// CreateTemp and Rename leaks one. Nothing ever reads these — the .json filter
// keeps them out of output — so this is litter collection, not correctness;
// the grace is what keeps it from unlinking a live writer's in-flight temp and
// failing that writer's Rename with ENOENT.
func sweepTmpOrphan(path string) {
	fi, err := os.Stat(path)
	if err != nil || time.Since(fi.ModTime()) < tmpOrphanGrace {
		return
	}
	_ = os.Remove(path)
}

// Peers lists recorded peers, newest-seen first. Dead ones (per the oracle:
// socket gone, pid gone, or argv mismatch) are unlinked as they are found —
// presence prunes itself, so no TTL sweep and no dependence on the lock-liveness
// check. Pass includeDead to see them anyway (they are dropped from disk
// regardless; the returned slice is the last observation, useful for "where did
// that session go").
//
// The same walk sweeps aged *.tmp crash debris (sweepTmpOrphan) — free, since
// the ReadDir already happens, and Peers is the declared self-pruning path.
func Peers(ctx context.Context, includeDead bool) ([]Peer, error) {
	entries, err := os.ReadDir(peersDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("peer registry: %w", err)
	}
	out := make([]Peer, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasSuffix(e.Name(), ".tmp") {
			sweepTmpOrphan(filepath.Join(peersDir(), e.Name()))
			continue
		}
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, ok := readPeerFile(ctx, filepath.Join(peersDir(), e.Name()), includeDead)
		if !ok {
			continue
		}
		out = append(out, p)
	}
	// Newest-seen first, uuid as the tiebreak so the order is total and the
	// output is reproducible.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].SeenAt.Equal(out[j].SeenAt) {
			return out[i].SeenAt.After(out[j].SeenAt)
		}
		return out[i].UUID < out[j].UUID
	})
	return out, nil
}
