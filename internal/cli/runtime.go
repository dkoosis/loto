package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
	"loto/internal/store"
)

// gitTimeout caps blocking git rev-parse calls so a hung repo (stale NFS,
// fsmonitor wedge) cannot turn the CLI into an unkillable process.
const gitTimeout = 5 * time.Second

// errNotInGitRepo is deliberately actionable (design.md: fix block under
// actionable findings) rather than surfacing the raw git exec error — "exit
// status 128" tells a Claude caller nothing it can act on. Repo-scoped
// commands (lock/status/tag/...) have no rendezvous point without a git
// toplevel to derive a project slug from, so this stays a hard fail; git init
// is the correct remedy, not a rootless fallback (loto-7wi).
var errNotInGitRepo = errors.New("not in a git repo\n  fix: git init   (loto coordinates within a repo; scratch work needs none)")

func gitRevParseToplevel(parent context.Context) (string, error) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// gitBranch resolves the holder's current branch for stamping onto lock
// records at acquire (loto-16cf): a peer's lock taken on another branch tells
// the reader their working tree is not yours. Detached HEAD records the short
// SHA. Best-effort by design — any git failure returns "" rather than block
// an acquire over a missing label.
func gitBranch(parent context.Context) string {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, gitTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(out))
	if name != "HEAD" {
		return name
	}
	out, err = exec.CommandContext(ctx, "git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isNotAGitRepo reports whether err is specifically git's own "not a
// repository" failure, as opposed to an infra failure — missing git binary,
// ctx timeout/cancellation, permission error — that must not be collapsed
// into the same case. Only *exec.ExitError carries captured stderr to check;
// a ctx-cancel/timeout or a LookPath failure surfaces as a different error
// type and correctly reads as false here (adversarial review on loto-7wi:
// the prior code mapped every gitRevParseToplevel error to "not in a git
// repo", misreporting real git/infra faults with a bogus `git init` remedy).
func isNotAGitRepo(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return bytes.Contains(exitErr.Stderr, []byte("not a git repository"))
}

type runtime struct {
	Agent         *identity.Agent
	Store         *store.Store
	Ctx           context.Context //nolint:containedctx // handle on the per-invocation ctx; threading it through every store/identity call from cmd_*.go would be uniform noise without changing semantics
	Host          string
	HostKnown     bool // false → this machine has no verifiable host id; liveProbe must not compare hosts (loto-u7e)
	StateDir      string
	RepoTop       string             // absolute repo toplevel; roots canonical target resolution
	SessionUUID   domain.SessionUUID // per-session id, distinct from Agent.UUID; sourced from LOTO_SESSION_ID or CLAUDE_CODE_SESSION_ID
	SessionPinned bool               // true iff either of those was in env; gates session-scoped semantics
	AgentPinned   bool               // true iff a non-empty LOTO_AGENT_ID, a usable LOTO_SUBAGENT_ID (SubagentIDPins), or CLAUDE_CODE_SESSION_ID pins an identity; false → Ensure minted a throwaway UUID
}

// sessionUUID resolves the per-session id. LOTO_SESSION_ID is the explicit
// override; CLAUDE_CODE_SESSION_ID is the id Claude Code already puts in the
// environment of every shell-out from one session. Either satisfies NORTH_STAR
// invariant 5 (per-session identity) and lets release --all scope to a session.
// With neither (single-shot CLI use), mint a fresh id but signal `pinned=false`
// so callers know not to use it as a release filter — keeps --all working as an
// agent-scoped fallback for direct invocation.
//
// ‡ The CLAUDE_CODE_SESSION_ID leg is not a convenience, it repairs a broken
// invariant (loto-2lj5). records.go documents SessionUUID as "one Claude
// session = one id, shared by every shell-out from that session", but nothing
// in this repo or in the cc-plugins hooks has ever exported LOTO_SESSION_ID, so
// the fallback minted a fresh UUID per INVOCATION. Measured in the live
// dkoosis-sdlc store, three locks taken by one session carried two different
// session ids and neither was the session's. Everything keyed on session
// identity was therefore inert: `unlock --session` (SessionPinned was always
// false), ReleaseBySession, and — the reason this is being fixed now — any
// attempt to ask whether a peer record speaks for a given record's session.
//
// identity.PeerFromEnv fills Peer.SessionID from identity.SessionIDFromEnv —
// the same function called here — so the two sides are comparable BY
// CONSTRUCTION rather than by convention. peerSpeaksFor below depends on
// exactly that, and the convention version had already drifted: the peer read
// CLAUDE_CODE_SESSION_ID alone while this preferred LOTO_SESSION_ID, so with
// the documented override set every DEAD verdict was discarded and PID-less
// locks and claims stayed blockers until their TTL (loto-37xm, Codex #248).
func sessionUUID() (id string, pinned bool) {
	if v := identity.SessionIDFromEnv(); v != "" {
		return v, true
	}
	return identity.NewUUID(), false
}

func openRuntime(ctx context.Context) (*runtime, error) {
	// Capture whether an explicit identity env var was set before Ensure runs.
	// Ensure mints a fresh throwaway UUID when neither is present; that UUID
	// owns no locks and must not be used as an --all release scope — doing so
	// produces a false-success that silently leaves real locks in place
	// (loto-pody). AgentPinned mirrors the SessionPinned pattern for sessions.
	// identity.PinnedByEnv is the single source shared with the gate probe and
	// Ensure's own precedence (loto-ai5), so this can't drift from resolution.
	agentPinned := identity.PinnedByEnv()

	a, err := identity.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	top, err := gitRevParseToplevel(ctx)
	if err != nil {
		if isNotAGitRepo(err) {
			return nil, errNotInGitRepo
		}
		return nil, fmt.Errorf("git rev-parse --show-toplevel: %w", err)
	}
	dir := StateDir(top)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s, err := store.OpenContext(ctx, filepath.Join(dir, "loto.db"))
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	// No identity GC here — openRuntime is the read path (check, check --gate,
	// guard, status) and must stay cheap. GC lives in openRuntimeGC (write
	// verbs) and doctor's explicit two-phase pass; sessionReferencedUUIDs walks
	// every ~/.loto/session/*.json, which cost 1.1s against a 15.5k-file dir on
	// this hot path (loto-6pn6).
	host, hostKnown := identity.HostID()
	if !hostKnown {
		// Announce the degradation: without a host id every lock recorded
		// under a real hostname reads UNKNOWN, so crashed holders wait for the
		// TTL backstop instead of being reclaimed on sight (loto-u7e).
		fmt.Fprintf(os.Stderr, "⚠ loto: hostname unavailable; stale-lock reclaim degraded to TTL only\n  fix: export LOTO_HOST=<stable-name-unique-to-this-machine>\n")
	}
	sid, pinned := sessionUUID()
	return &runtime{
		Agent:         a,
		Store:         s,
		Ctx:           ctx,
		Host:          host,
		HostKnown:     hostKnown,
		StateDir:      dir,
		RepoTop:       top,
		SessionUUID:   domain.SessionUUID(sid),
		SessionPinned: pinned,
		AgentPinned:   agentPinned,
	}, nil
}

// openRuntimeGC is openRuntime plus the identity-registry GC pass, for verbs
// that mutate coordination state (lock/unlock/tag/ack/claim/unclaim/lane/
// downgrade/refresh). Read verbs — check, check --gate, guard, status — must
// NOT use it: sessionReferencedUUIDs walks every ~/.loto/session/*.json, which
// cost 1.1s on the PreToolUse gate path against a 15.5k-file dir (loto-6pn6).
// GC runs here, before the caller mutates anything: lockOwnerUUIDs must see
// the lock rows that pin their owner agents, and an unlock --all has already
// dropped them by the time it returns (gh#125 / loto-ffg).
func openRuntimeGC(ctx context.Context) (*runtime, error) {
	rt, err := openRuntime(ctx)
	if err != nil {
		return nil, err
	}
	_ = identity.GCAgents(time.Now(), lockOwnerUUIDs(ctx, rt.Store))
	return rt, nil
}

func repoTopForCwd(ctx context.Context) (string, error) {
	return gitRevParseToplevel(ctx)
}

// Close releases the per-project store, if one was opened. A runtime built
// without a store leaves Store nil, and Close is then a no-op rather than a
// nil-pointer panic.
func (r *runtime) Close() error {
	if r.Store == nil {
		return nil
	}
	return r.Store.Close()
}

// DeferredTagFooter prints, after the primary command output, both kinds of
// note the caller ought to see: pending external tags on locks it holds, and
// territory tags pinned to ground it now holds (loto-z3y1).
//
// Opted-in commands (lock, unlock, refresh, claim, unclaim, status, doctor)
// register this via `defer`; check is excluded deliberately — its output is a
// pinned machine surface for trixi's PreToolUse hook, and it gets its own
// advisory block instead.
//
// ‡ The `defer` placement is what makes the territory half work at all: the
// footer runs AFTER the command's mutation has committed, so a just-acquired
// lock is already a row by the time we ask what ground the caller holds.
// Extending the body reaches all seven verbs without touching one call site.
//
// Tags whose host lock disappeared mid-command (release, break) are filtered by
// the JOIN inside ListAliveForOwner.
func (r *runtime) DeferredTagFooter(w io.Writer) {
	tags, err := r.Store.ListAliveForOwner(r.Ctx, domain.AgentUUID(r.Agent.UUID))
	if err == nil && len(tags) > 0 {
		render.EmitTagFooter(w, tags, r.Agent.UUID)
	}
	now := time.Now()
	emitTerritoryTagFooter(w, territoryTagsForHeldGround(r, liveTerritoryTags(r, now), now))
}

// liveProbe returns the HolderLiveProbe every staleness predicate consumes:
// oracle first, pid fallback. Centralizes the live-probe closure that otherwise
// gets re-built at every lock/unlock/doctor call site.
//
// Layer 1 — the session-liveness oracle (identity.AgentLive, loto-ygty), keyed
// on the lock's owner uuid. A LIVE verdict overrides the pid probe entirely:
// a worktree/subagent holder whose stamped pid probes dead is still alive if
// its session socket + ps identity check out (loto-r11w). A DEAD verdict
// fast-reclaims without touching the pid.
//
// Layer 2 — pid fallback, when the oracle has no peer record. Remote-host and
// PID-0 locks are UNKNOWN (TTL is the sole authority — loto-t1tq/loto-j1bo).
// Degraded mode (loto-u7e): when this process has no verifiable host id every
// lock is UNKNOWN, including host-less records — two machines that both failed
// the lookup would otherwise compare equal and pid-probe each other's pids.
// Reclaim then rests entirely on the TTL backstop until LOTO_HOST is set. The
// bias is deliberate: a false DEAD breaks a live lock and lets two agents write
// one file, which is unrecoverable, while a false UNKNOWN only delays reclaim.
//
// PID-reuse defense (loto-kwlp): for a live local pid with a known start-time,
// the occupant's start-time is re-read; a mismatch means the OS recycled the
// pid → DEAD. Indeterminate start-time reads never escalate — the pid already
// probed alive.
func (r *runtime) liveProbe() domain.HolderLiveProbe {
	return func(l domain.LockRecord) domain.Liveness {
		if !r.HostKnown || l.Host != r.Host {
			return domain.LivenessUnknown
		}
		v := identity.AgentLive(r.Ctx, string(l.OwnerUUID))
		switch v.Liveness {
		case identity.SessionLive:
			return domain.LivenessAlive
		case identity.SessionDead:
			if peerSpeaksFor(v.Peer, l.SessionUUID) {
				return domain.LivenessDead
			}
			// The peer record belongs to a DIFFERENT session of the same
			// agent, so its death is no evidence about this record's holder.
			// Fall through (loto-2lj5).
		case identity.SessionUnknown:
			// no peer record — fall through to the pid probe
		}
		return pidVerdict(l)
	}
}

// peerSpeaksFor reports whether a peer record is evidence about the session
// that took this record. Peer records are keyed on agent uuid alone
// (identity.peerPath), but sibling sessions sharing one LOTO_AGENT_ID are a
// supported, tested configuration (loto-81n) — so one agent uuid can have
// several live sessions and exactly one peer record. When a sibling dies, that
// record reads DEAD, and without this check every lock held by the still-live
// siblings is classified stale: two agents write one file, the one failure
// loto exists to prevent and the one direction that cannot be recovered from.
//
// ‡ Asymmetric by design, and the asymmetry is the safety argument. Only the
// DEAD verdict is gated. A LIVE verdict from the wrong sibling keeps a lock
// denying, which over-refuses — annoying, reversible, and strictly the safe
// direction. Gating both would be tidier and would give up the loto-r11w
// override, where a worktree/subagent holder whose stamped pid probes dead is
// still alive because its session socket checks out.
//
// Both ids empty means the caller is not a Claude session at all (direct CLI
// use, legacy rows): nothing to correspond, nothing to contradict, so the
// verdict stands as before. A mismatch falls through to pidVerdict, which
// answers from the record's own stamped pid, or UNKNOWN when there is none —
// and then the TTL is the sole authority, exactly as the bead asks.
func peerSpeaksFor(p *identity.Peer, session domain.SessionUUID) bool {
	if p == nil {
		return false
	}
	return p.SessionID == string(session)
}

// memoLiveProbe wraps a probe so each distinct holder is probed at most once
// per CLI invocation (Codex #246). The gate evaluates its predicate per
// (target × record), and a probe is not free: identity.AgentLive reads a peer
// record off disk and the pid fallback can shell out to `ps`. One repo-root
// claim over a 300-file staged set therefore paid 300 identical probes on the
// pre-commit hot path.
//
// The key carries everything the probe reads — host, owner, SESSION, pid,
// proc-start — so two records that differ in any of them stay independently
// probed. The cache lives for one command run, well inside the window where a
// holder's liveness could meaningfully change, and a gate that answered ALIVE
// for one target and DEAD for the next in the same verdict would be incoherent
// anyway.
//
// ‡ The session leg is not decoration (loto-s0bb, Codex #248). liveProbe reads
// l.SessionUUID through peerSpeaksFor: one agent uuid can carry several live
// sibling sessions and exactly one peer record, so the SAME (host, owner, pid,
// proc-start) yields DEAD for the session the record names and UNKNOWN for its
// siblings. Sibling beacons and every claim share PID 0, which made them
// collide on the old key: probe the dead sibling first and its DEAD verdict was
// served to a live one, so check --gate, guard, and claim acquisition would all
// hand a live agent's territory to a competing writer. That is the one loto
// failure with no recovery — a false UNKNOWN only delays a reclaim.
func memoLiveProbe(p domain.HolderLiveProbe) domain.HolderLiveProbe {
	if p == nil {
		return nil
	}
	type key struct {
		host, owner, session string
		pid                  int
		procStart            int64
	}
	seen := map[key]domain.Liveness{}
	return func(l domain.LockRecord) domain.Liveness {
		k := key{
			host:      l.Host,
			owner:     string(l.OwnerUUID),
			session:   string(l.SessionUUID),
			pid:       l.PID,
			procStart: l.ProcStart,
		}
		if v, ok := seen[k]; ok {
			return v
		}
		v := p(l)
		seen[k] = v
		return v
	}
}

// pidVerdict is liveProbe's layer-2 fallback for a local lock with no peer
// record: PID-0 sentinel → unknown (TTL governs), dead pid → dead, live pid
// with a recycled start-time (loto-kwlp) → dead, else alive.
//
// identity.ProcStart is the same reader the peer oracle now uses for its own
// start-time witness (loto-uxhg, identity.SessionVerdict) — one implementation
// for both liveness paths in the codebase.
func pidVerdict(l domain.LockRecord) domain.Liveness {
	if l.PID <= 0 {
		return domain.LivenessUnknown
	}
	if !identity.PIDAlive(l.PID) {
		return domain.LivenessDead
	}
	if l.ProcStart != 0 {
		if cur, ok := identity.ProcStart(l.PID); ok && cur != l.ProcStart {
			return domain.LivenessDead
		}
	}
	return domain.LivenessAlive
}

// lockOwnerUUIDs collects the set of owner_uuid values referenced by live
// lock rows in s. Fed to identity.GCAgents so the agent-registry GC pass
// pins agents that any live lock still depends on, closing gh#125
// (loto-ffg). A ListLocks failure is non-fatal: a nil set means GC runs
// with only its built-in session-cache pin set — the worst case is the
// same as the pre-fix behavior, not a regression.
func lockOwnerUUIDs(ctx context.Context, s *store.Store) map[string]struct{} {
	locks, err := s.ListLocks(ctx)
	if err != nil {
		return nil
	}
	out := make(map[string]struct{}, len(locks))
	for i := range locks {
		if locks[i].OwnerUUID != "" {
			// GCAgents takes a plain map[string]struct{}: the identity package
			// is pinned to internal/identity → ∅ and cannot reference
			// domain.AgentUUID, so the owner crosses back to a string here.
			out[string(locks[i].OwnerUUID)] = struct{}{}
		}
	}
	return out
}
