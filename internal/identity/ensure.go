package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	// A stamped subagent resolves first, so a /team sibling diverges from the
	// session identity it inherits from its parent (see resolveSubagent).
	if a, handled := resolveSubagent(); handled {
		return a, nil
	}
	return ensureUnstamped()
}

// Ephemeral mints a throwaway in-memory owner for display-only use. It owns
// nothing and nothing ever resolves back to it; a read verb on a bare shell
// (`loto status`, `loto check`) uses it so the table still renders.
func Ephemeral() *Agent {
	host, _ := HostID()
	return &Agent{UUID: newUUID(), Host: host}
}

// EnsureParent resolves the identity a LOTO_SUBAGENT_ID stamp is hiding: the
// owner this process would be WITHOUT the stamp (loto-wofb). A stamped
// sibling's Bash calls are unstamped — hooks cannot export env into later
// tool calls — so its own `loto lock`/`claim` rows are owned by this parent
// identity. The gate treats those as the sibling's own; otherwise a worker is
// refused by the very lock it just took.
//
// handled=false when no stamp pins an identity (nothing is hidden) or when the
// parent env does not pin one either.
func EnsureParent(ctx context.Context) (*Agent, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if !SubagentIDPins(os.Getenv("LOTO_SUBAGENT_ID")) {
		return nil, false, nil
	}
	if !envIdentityBinding().pinsForAuthority() {
		return nil, false, nil
	}
	a, err := ensureUnstamped()
	if err != nil {
		return nil, false, err
	}
	return a, true, nil
}

// SubagentIDPins reports whether a LOTO_SUBAGENT_ID value would actually pin
// an identity via resolveSubagent. A stamp pins whenever the UNSTAMPED
// environment already pins a deterministic owner to derive from — either
// binding, not the session id alone: a dispatcher that pins LOTO_AGENT_ID and
// stamps each sibling must still get distinct owners (loto-fs84), which a
// session-only precondition silently collapsed. An empty stamp, a throwaway
// (set-but-empty LOTO_AGENT_ID, whose owner is random per invocation and so
// cannot key a stable sibling) and an unbound env all fall open. Callers
// deciding fail-open vs fail-closed (`loto check --gate`) must not treat
// those as pinned.
//
// The stamp itself needs no validation: deriveUUID hashes it, so it never
// names a path or reaches a render surface.
func SubagentIDPins(id string) bool {
	if id == "" {
		return false
	}
	_, ok := subagentParentOwner()
	return ok
}

// subagentParentOwner returns the deterministic owner id a stamped sibling
// derives from, and whether one exists. Both bindings qualify — a dispatcher
// that pins LOTO_AGENT_ID and stamps each sibling must get distinct owners
// just as a session-bound one does (loto-fs84).
//
// A blank LOTO_AGENT_ID does NOT suppress the stamp: loto-s3l bars the session
// id from RESCUING a blank agent id on the UNSTAMPED path, where the caller
// asked for a throwaway; a stamp is a deliberate per-sibling signal from the
// dispatch hook, and the session id is the stable key it derives against.
// A throwaway owner would be random per invocation and could not key a
// sibling that must re-derive the same owner on every call.
func subagentParentOwner() (string, bool) {
	if v, set := os.LookupEnv("LOTO_AGENT_ID"); set && v != "" {
		if !agentIDShape.MatchString(v) {
			return "", false // malformed pin: fall open so Ensure reports it
		}
		return v, true
	}
	sid := os.Getenv("CLAUDE_CODE_SESSION_ID")
	if sid == "" {
		return "", false
	}
	if _, err := sessionPath(sid); err != nil {
		return "", false
	}
	if !sessionIDShape.MatchString(sid) {
		return "", false
	}
	return sid, true
}

// resolveSubagent resolves a stamped LOTO_SUBAGENT_ID to a stable per-sibling
// owner. handled=true means Ensure returns a; handled=false means fall open to
// normal resolution.
//
// A /team subagent inherits the parent's identity env, collapsing every sibling
// onto one owner; loto then reads a sibling's lock as a re-entrant TTL refresh
// and never serializes the collision (loto-fs84, loto-wbkn). The PreToolUse
// hook stamps the per-subagent CC agent_id — distinct per sibling, null at
// root — into LOTO_SUBAGENT_ID, and the owner is derived from (parent owner
// id, stamp) so siblings get distinct owners the existing conflict logic
// serializes, with no cache file to race for. Deriving from the PARENT OWNER
// rather than the session id keeps the LOTO_AGENT_ID-pinned dispatcher case
// working too.
//
// Fail-open by contract: the agent_id field is undocumented and may vanish on
// a CC upgrade, and the stamp is only a backstop to dispatch write-set
// partitioning — never load-bearing. An absent, malformed, or session-less
// stamp falls open (handled=false) rather than erroring.
func resolveSubagent() (*Agent, bool) {
	sub := os.Getenv("LOTO_SUBAGENT_ID")
	if !SubagentIDPins(sub) {
		return nil, false
	}
	parent, ok := subagentParentOwner()
	if !ok {
		return nil, false
	}
	host, _ := HostID()
	return &Agent{UUID: deriveUUID(parent, sub), Host: host}, true
}

// ensureUnstamped is Ensure minus the subagent stamp: the env-binding
// resolution every identity-bearing process goes through.
func ensureUnstamped() (*Agent, error) {
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
		return &Agent{UUID: sid, Host: host}, nil
	default: // bindingUnbound
		return nil, ErrUnpinned
	}
}

// envIdentityBinding classifies the post-subagent identity precedence purely
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
	if SubagentIDPins(os.Getenv("LOTO_SUBAGENT_ID")) {
		return true
	}
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
