package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
)

var (
	errInvalidAgentID = errors.New("invalid LOTO_AGENT_ID")

	// ErrUnpinned is Ensure's answer when nothing in the environment names an
	// owner. Authority-bearing verbs refuse on it; read-only verbs substitute
	// Ephemeral() (internal/cli/runtime.go openRuntime).
	ErrUnpinned = errors.New("identity unpinned: set CLAUDE_CODE_SESSION_ID (Claude Code exports it) or LOTO_AGENT_ID=<uuid>; callers outside Claude Code are not supported yet")
)

// Agent is the owner identity every lock, claim and tag is attributed to.
// UUID is the value the store's owner_uuid column carries; Host is the
// machine the process resolved on (HostID), for display.
type Agent struct {
	UUID string `json:"uuid"`
	Host string `json:"host"`
}

// Ensure resolves the current agent identity by the contract documented in
// the package doc. Pure env read — no disk IO, no minting of anything that
// persists. Returns ErrUnpinned when nothing pins an owner.
func Ensure(ctx context.Context) (*Agent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return ensureEnv()
}

// Ephemeral mints a throwaway in-memory owner for display-only use. It owns
// nothing and nothing ever resolves back to it; a read verb on a bare shell
// (`loto status`, `loto check`) uses it so the table still renders.
func Ephemeral() *Agent {
	host, _ := HostID()
	return &Agent{UUID: newUUID(), Host: host}
}

// ensureEnv is the env-binding resolution every identity-bearing process goes
// through.
func ensureEnv() (*Agent, error) {
	host, _ := HostID()
	// The precedence — LOTO_AGENT_ID (explicit vs blank-ephemeral) →
	// CLAUDE_CODE_SESSION_ID → unbound — is classified in ONE place so the
	// fail-open/closed gates read the same decision Ensure resolves by, instead
	// of re-deriving it (loto-ai5; the earlier drift was loto-s3l's blank id).
	switch envIdentityBinding() { //nolint:exhaustive // default handles the only remaining case, bindingUnbound
	case bindingAgentIDExplicit:
		u := os.Getenv("LOTO_AGENT_ID")
		if !agentIDShape.MatchString(u) {
			return nil, fmt.Errorf("%w: %q (want canonical uuid hex form)", errInvalidAgentID, u)
		}
		return &Agent{UUID: u, Host: host}, nil
	case bindingAgentIDEphemeral:
		// Set-but-empty LOTO_AGENT_ID is explicit-ephemeral: mint a throwaway
		// and STOP — never fall through to the session id below (loto-s3l).
		return &Agent{UUID: newUUID(), Host: host}, nil
	case bindingSession:
		sid := os.Getenv("CLAUDE_CODE_SESSION_ID")
		// Same traversal guard the session record file gets, so an owner id
		// can never name a path outside sessionDir. Claude Code emits UUIDs;
		// the shape is deliberately NOT enforced beyond that, because a test
		// harness or an override may pin any opaque token here.
		if _, err := sessionPath(sid); err != nil {
			return nil, fmt.Errorf("%w: CLAUDE_CODE_SESSION_ID=%q", err, sid)
		}
		// sessionPath only guards the filename. The same value also becomes a
		// git author ident and a field in single-line render rows, so it must
		// carry no space, control byte or ident metacharacter (loto-jnid
		// review). Still not a UUID check — see sessionIDShape.
		if !sessionIDShape.MatchString(sid) {
			return nil, fmt.Errorf("%w: CLAUDE_CODE_SESSION_ID=%q (want [A-Za-z0-9._-], 1-128 chars)", errInvalidSessionID, sid)
		}
		return &Agent{UUID: actorOwner(sid, host), Host: host}, nil
	default: // bindingUnbound
		return nil, ErrUnpinned
	}
}

// actorOwner is the owner id of the actor behind session sid: the Claude Code
// process, (host, pid, process start time) (docs/write-gate.md, loto-e1ku).
// /clear rotates the session id and leaves the process up, and every
// subagent, in-process or isolation: worktree, runs inside it, so all of them
// resolve to one owner. The process comes from env first (the sources
// RecordSession stamps a record with), then from sid's session record, which
// is how a hook spawned without the session's env still names the same
// actor. With neither, or with no start time to keep a recycled pid apart,
// the process is unknown and the owner is sid itself.
//
// ‡ CLAUDE_PID before LOTO_PID, the reverse of sessionPID. Claude Code sets
// CLAUDE_PID for each process it spawns, while LOTO_PID is a Bash export that
// a claude started from that Bash inherits: read first, it would hand the
// child's hooks its parent's actor.
func actorOwner(sid, host string) string {
	p := actorProc()
	if p.pid <= 0 || p.procStart == 0 {
		if rec, ok := readSession(sid); ok {
			p = procIdent{pid: rec.PID, procStart: rec.ProcStart}
		}
	}
	if p.pid <= 0 || p.procStart == 0 {
		return sid
	}
	return deriveUUID(host, fmt.Sprintf("%d/%d", p.pid, p.procStart))
}

// actorProc is callerProc with Claude Code's own pid first (see actorOwner).
func actorProc() procIdent {
	if n, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && n > 0 {
		procStart, _ := procStartFn(n)
		return procIdent{pid: n, procStart: procStart}
	}
	return callerProc()
}

// PinnedByAgentID reports whether LOTO_AGENT_ID names the owner outright. That
// owner is not a process, so one id may span many sessions (sd-xhap).
func PinnedByAgentID() bool {
	return envIdentityBinding() == bindingAgentIDExplicit
}

// envIdentityBinding classifies the identity precedence purely
// from env — no store IO. It is the single home for the LOTO_AGENT_ID / session
// ordering that both Ensure (which resolves each case) and PinnedByEnv (which
// only asks "does this pin authority?") consume, so the two can no longer drift.
//
// Ordering is load-bearing: LOTO_AGENT_ID set-ness is checked BEFORE the session
// id, because a set-but-empty agent id is explicit-ephemeral and must NOT be
// rescued by a present session id (loto-s3l P1).
type envBinding int

const (
	bindingUnbound          envBinding = iota // no identity env → ErrUnpinned
	bindingAgentIDExplicit                    // LOTO_AGENT_ID set & non-empty → the pin, or a hard error on bad shape
	bindingAgentIDEphemeral                   // LOTO_AGENT_ID set & empty → throwaway
	bindingSession                            // CLAUDE_CODE_SESSION_ID set → stable per-session owner
)

func envIdentityBinding() envBinding {
	if v, set := os.LookupEnv("LOTO_AGENT_ID"); set {
		if v == "" {
			return bindingAgentIDEphemeral
		}
		return bindingAgentIDExplicit
	}
	if os.Getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return bindingSession
	}
	return bindingUnbound
}

// pinsForAuthority reports whether this binding makes Ensure resolve a real,
// lock-owning agent (or hard-error) rather than mint a throwaway or refuse.
func (b envBinding) pinsForAuthority() bool {
	return b == bindingAgentIDExplicit || b == bindingSession
}

// PinnedByEnv reports whether the environment pins an authority-bearing identity
// — i.e. Ensure will resolve a real, lock-owning agent (or hard-error), NOT mint
// a throwaway or refuse. Pure env read, so `loto check --gate` can fail-open
// BEFORE opening the store on a bare human shell, write verbs can refuse, and
// `release --all` can fail-closed, all off the SAME precedence Ensure
// dispatches on (loto-ai5, loto-s3l).
func PinnedByEnv() bool {
	return envIdentityBinding().pinsForAuthority()
}

// SessionIDFromEnv resolves the per-session id from the environment, empty
// when neither variable is set. LOTO_SESSION_ID is the explicit override;
// CLAUDE_CODE_SESSION_ID is the id Claude Code puts in the environment of
// every shell-out from one session.
//
// ‡ This is the ONE precedence, shared by the session record written by
// RecordSession and by the session id stamped on lock and claim rows
// (cli.sessionUUID). The two sides are compared — liveProbe asks ProbeSession
// about the session that took a given record — so a record keyed from a
// different variable than the row silently breaks that lookup (loto-37xm,
// Codex #248).
func SessionIDFromEnv() string {
	if v := os.Getenv("LOTO_SESSION_ID"); v != "" {
		return v
	}
	return os.Getenv("CLAUDE_CODE_SESSION_ID")
}
