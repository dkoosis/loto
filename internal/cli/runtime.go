package cli

import (
	"context"
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

type runtime struct {
	Agent         *identity.Agent
	Store         *store.Store
	Ctx           context.Context //nolint:containedctx // handle on the per-invocation ctx; threading it through every store/identity call from cmd_*.go would be uniform noise without changing semantics
	Host          string
	StateDir      string
	SessionUUID   string // per-session id, distinct from Agent.UUID; sourced from LOTO_SESSION_ID
	SessionPinned bool   // true iff LOTO_SESSION_ID was in env; gates session-scoped semantics
}

// sessionUUID resolves the per-session id. The SessionStart hook exports
// LOTO_SESSION_ID so every shell-out from one Claude session shares an id
// distinct from Agent.UUID; release --all then scopes to that session,
// satisfying NORTH_STAR invariant 5 (per-session identity). Without the env
// var (single-shot CLI use), mint a fresh id but signal `pinned=false` so
// callers know not to use it as a release filter — keeps --all working as
// an agent-scoped fallback for direct invocation.
func sessionUUID() (id string, pinned bool) {
	if v := os.Getenv("LOTO_SESSION_ID"); v != "" {
		return v, true
	}
	return identity.NewUUID(), false
}

func openRuntime(ctx context.Context) (*runtime, error) {
	a, err := identity.Ensure()
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	dir, err := stateDirForCwd(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s, err := store.OpenContext(ctx, filepath.Join(dir, "loto.db"))
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	host, _ := os.Hostname()
	sid, pinned := sessionUUID()
	return &runtime{
		Agent:         a,
		Store:         s,
		Ctx:           ctx,
		Host:          host,
		StateDir:      dir,
		SessionUUID:   sid,
		SessionPinned: pinned,
	}, nil
}

func repoTopForCwd(ctx context.Context) (string, error) {
	return gitRevParseToplevel(ctx)
}

func (r *runtime) Close() error { return r.Store.Close() }

// DeferredTagFooter prints the caller-as-holder's pending external tags after
// the primary command output. Opted-in commands (lock, unlock, status, doctor)
// register this via `defer`; check is excluded deliberately — its output is a
// pinned machine surface for trixi's PreToolUse hook.
//
// Tags whose host lock disappeared mid-command (release, break) are filtered by
// the JOIN inside ListAliveForHolder.
func (r *runtime) DeferredTagFooter(w io.Writer) {
	tags, err := r.Store.ListAliveForHolder(r.Ctx, r.Agent.UUID)
	if err != nil || len(tags) == 0 {
		return
	}
	render.EmitTagFooter(w, tags, r.Agent.UUID)
}

// liveProbe returns a PidLiveProbe that treats remote-host PIDs as live and
// probes local PIDs via pidLive. Centralizes the live-probe closure that
// otherwise gets re-built at every lock/unlock/doctor call site.
func (r *runtime) liveProbe() domain.PidLiveProbe {
	return func(host string, pid int) bool {
		if host != r.Host {
			return true
		}
		return pidLive(pid)
	}
}

func stateDirForCwd(ctx context.Context) (string, error) {
	top, err := gitRevParseToplevel(ctx)
	if err != nil {
		return "", fmt.Errorf("not in a git repo: %w", err)
	}
	return StateDir(top), nil
}
