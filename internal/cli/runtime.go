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
	Agent    *identity.Agent
	Store    *store.Store
	Ctx      context.Context //nolint:containedctx // request-scope handle for the CLI invocation; threading it through every cmd_*.go signature would be uniformly noise
	Host     string
	StateDir string
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
	if repoTop, err := repoTopForCwd(); err == nil {
		_, _ = s.FSCaseSensitive(repoTop)
	}
	host, _ := os.Hostname()
	return &runtime{Agent: a, Store: s, Ctx: runtimeCtx(), Host: host, StateDir: dir}, nil
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
