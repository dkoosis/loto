package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"loto/internal/identity"
	"loto/internal/store"
)

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
	host, _ := os.Hostname()
	return &runtime{Agent: a, Store: s, Ctx: context.Background(), Host: host, StateDir: dir}, nil
}

func repoTopForCwd() (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (r *runtime) Close() error { return r.Store.Close() }

func stateDirForCwd() (string, error) {
	out, err := exec.CommandContext(context.Background(), "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repo: %w", err)
	}
	top := strings.TrimSpace(string(out))
	return StateDir(top), nil
}
