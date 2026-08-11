package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
}

// Named reports whether this peer is addressable by SendMessage today.
func (p Peer) Named() bool { return p.Name != "" }

// Live reports whether the session behind this peer is still up, per the one
// liveness oracle (SessionVerdict, liveness.go). Kept as the boolean
// convenience for callers that only branch; unknown maps to true — a verdict
// this record cannot support must not read as dead.
func (p Peer) Live(ctx context.Context) bool {
	return p.SessionVerdict(ctx).Liveness != SessionDead
}

func peersDir() string { return filepath.Join(homeDir(), ".loto", "peers") }

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

// procArgv returns the full command line of pid, or "" if it cannot be read.
// Var, not func, so tests can drive the parse without a real process.
var procArgv = psArgv //nolint:gochecknoglobals // test seam for the proc-table read

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
	body, err := os.ReadFile(path)
	if err != nil {
		return Peer{}, false
	}
	var p Peer
	if err := json.Unmarshal(body, &p); err != nil {
		return Peer{}, false
	}
	return p, true
}

// readPeerFile loads one peer record, unlinking it when the oracle says the
// session is DEAD — presence prunes itself. An UNKNOWN verdict keeps the
// record: no witness either way is not evidence of death. ok=false means "skip
// this entry": unreadable, unparseable, or dead and not asked for. An
// unreadable record is skipped rather than surfaced — a peer table is a
// convenience, and half of one still beats an error.
func readPeerFile(ctx context.Context, path string, includeDead bool) (Peer, bool) {
	p, ok := readPeerRaw(path)
	if !ok {
		return Peer{}, false
	}
	if p.SessionVerdict(ctx).Liveness != SessionDead {
		return p, true
	}
	_ = os.Remove(path)
	return p, includeDead
}

// Peers lists recorded peers, newest-seen first. Dead ones (per the oracle:
// socket gone, pid gone, or argv mismatch) are unlinked as they are found —
// presence prunes itself, so no TTL sweep and no dependence on the lock-liveness
// check. Pass includeDead to see them anyway (they are dropped from disk
// regardless; the returned slice is the last observation, useful for "where did
// that session go").
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
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
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
