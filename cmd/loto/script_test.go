package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"

	"loto/internal/cli"
)

// TestMain wires the `loto` binary into testscript so scripts can invoke it
// in-process. Without this, `loto` calls inside a script would shell out to
// whatever's on PATH.
func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){
		"loto": func() {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			os.Exit(cli.RunContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
		},
	})
}

// TestScripts runs every *.txtar under testdata/script.
func TestScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata/script",
		Setup: func(env *testscript.Env) error {
			// Per-script HOME so agent registries don't collide across parallel
			// runs. LOTO_BASE separated so we can blow it away without nuking
			// HOME-side caches.
			home := filepath.Join(env.WorkDir, ".home")
			base := filepath.Join(env.WorkDir, ".lotobase")
			for _, d := range []string{home, base} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			env.Setenv("HOME", home)
			env.Setenv("LOTO_BASE", base)
			env.Setenv("XDG_STATE_HOME", "")
			// Clear inherited identity state so each script starts clean.
			env.Setenv("LOTO_AGENT_ID", "")
			env.Setenv("CLAUDE_CODE_SESSION_ID", "")
			// Stamp locks with the long-lived test binary PID so the staleness
			// probe doesn't reclaim Alice's lock the instant her `loto` subprocess
			// exits.
			env.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
			// Absolute path to this repo's real .githooks/ dir, so a script can
			// `git config core.hooksPath $REALHOOKS` and exercise the actual
			// tracked pre-commit dispatcher (ccp-vx4w) — not a hand-copied
			// stand-in that could drift from what ships.
			realHooks, err := filepath.Abs(filepath.Join("..", "..", ".githooks"))
			if err != nil {
				return err
			}
			env.Setenv("REALHOOKS", realHooks)

			// Two personas scripts swap between via `env LOTO_AGENT_ID=$ALICE`.
			// An explicit LOTO_AGENT_ID is the owner with nothing on disk to
			// resolve it against (loto-jnid), so a fixed uuid per persona is
			// the whole fixture — scripts assert on `$ALICE` where they used
			// to assert on a handle.
			env.Setenv("ALICE", "aaaaaaaa-0000-4000-8000-00000000a11c")
			env.Setenv("BOB", "bbbbbbbb-0000-4000-8000-000000000b0b")

			// Init a git repo at $WORK so loto's repo-root resolver finds one.
			return gitInit(env.WorkDir)
		},
		Cmds: map[string]func(ts *testscript.TestScript, neg bool, args []string){
			// `touch <path>` — create empty files for lock targets.
			"touch": func(ts *testscript.TestScript, neg bool, args []string) {
				if len(args) == 0 {
					ts.Fatalf("usage: touch <path>...")
				}
				for _, p := range args {
					full := ts.MkAbs(p)
					if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
						ts.Fatalf("mkdir: %v", err)
					}
					f, err := os.Create(full)
					if err != nil {
						ts.Fatalf("create: %v", err)
					}
					f.Close()
				}
				if neg {
					ts.Fatalf("touch unexpectedly succeeded")
				}
			},
		},
	})
}

func gitInit(dir string) error {
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "T"},
		{"remote", "add", "origin", "git@github.com:test/proj.git"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w\n%s", args, err, out)
		}
	}
	return nil
}
