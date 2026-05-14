package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/identity"
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
	Agent          *identity.Agent
	Store          *store.Store
	Ctx            context.Context //nolint:containedctx // request-scope handle for the CLI invocation; threading it through every cmd_*.go signature would be uniformly noise
	Host           string
	StateDir       string
	SessionUUID    string // per-session id, distinct from Agent.UUID; sourced from LOTO_SESSION_ID
	SessionPinned  bool   // true iff LOTO_SESSION_ID was in env; gates session-scoped semantics
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

func openRuntime() (*runtime, error) {
	a, err := identity.Ensure()
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	dir, err := stateDirForCwd()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s, err := store.Open(filepath.Join(dir, "loto.db"))
	if err != nil {
		return nil, fmt.Errorf("store.Open: %w", err)
	}
	host, _ := os.Hostname()
	sid, pinned := sessionUUID()
	return &runtime{
		Agent:         a,
		Store:         s,
		Ctx:           runtimeCtx(),
		Host:          host,
		StateDir:      dir,
		SessionUUID:   sid,
		SessionPinned: pinned,
	}, nil
}

func repoTopForCwd() (string, error) {
	return gitRevParseToplevel(runtimeCtx())
}

func (r *runtime) Close() error { return r.Store.Close() }

func stateDirForCwd() (string, error) {
	top, err := gitRevParseToplevel(runtimeCtx())
	if err != nil {
		return "", fmt.Errorf("not in a git repo: %w", err)
	}
	return StateDir(top), nil
}
