package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

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
			env.Setenv("LOTO_HANDLE", "")
			// Stamp locks with the long-lived test binary PID so the staleness
			// probe doesn't reclaim Alice's lock the instant her `loto` subprocess
			// exits.
			env.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))

			// Pre-mint two persisted agents so scripts can swap personas via
			// `env LOTO_AGENT_ID=$ALICE`. Written directly to disk to avoid
			// racing on os.Setenv across parallel scripts.
			alice, err := mintAgentFile(home, "AliceTester")
			if err != nil {
				return err
			}
			bob, err := mintAgentFile(home, "BobTester")
			if err != nil {
				return err
			}
			env.Setenv("ALICE", alice)
			env.Setenv("BOB", bob)

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

func mintAgentFile(home, handle string) (string, error) {
	type agent struct {
		UUID      string    `json:"uuid"`
		Handle    string    `json:"handle"`
		CreatedAt time.Time `json:"created_at"`
		Host      string    `json:"host"`
	}
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	uuid := "00000000-0000-4000-8000-" + hex.EncodeToString(buf[:])
	a := agent{UUID: uuid, Handle: handle, CreatedAt: time.Now().UTC(), Host: "testscript"}
	dir := filepath.Join(home, ".loto", "agents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, uuid+".json"), body, 0o600); err != nil {
		return "", err
	}
	return uuid, nil
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
