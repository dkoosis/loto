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

// realHomeAtProcessStart captures $HOME exactly as the test binary inherited
// it, before TestMain below ever touches the env var — testHomeGuardCanary
// uses it to prove the floor below is actually in effect.
var realHomeAtProcessStart = os.Getenv("HOME")

// TestMain wires the `loto` binary into testscript so scripts can invoke it
// in-process. Without this, `loto` calls inside a script would shell out to
// whatever's on PATH.
//
// It also installs loto-bt6c's structural backstop for this package: every
// TestScripts run already redirects HOME per-script (its own Setup below),
// but that per-script env only ever reaches the "loto" subcommand dispatch —
// nothing stops some future non-testscript test in this package (a plain
// TestXxx added later) from calling into internal/cli directly and, forgetting
// its own isolation, resolving cli.StateDir straight into dk's real
// ~/.local/state/loto/projects (loto-reuo, the sibling directory to the
// ~/.loto agents/session leak loto-bt6c fixed). internal/cli, internal/render
// and internal/identity already carry this same floor; cmd/loto — the package
// that ships the actual binary entrypoint — did not.
//
// Redirecting HOME alone is not enough: cli.StateDir's xdgStateHome
// (internal/cli/paths.go) prefers $XDG_STATE_HOME over $HOME and only
// consults the home dir when it is unset, and LOTO_BASE overrides StateDir
// outright ahead of both. A shell (or CI) that exports either would still
// steer a direct internal/cli call at the real state dir despite the HOME
// redirect below (cubic P2, PR #384) — so the floor clears both too.
//
// ‡ The floor below runs ONLY when this process is the top-level `go test`
// binary, never when it is the re-exec'd "loto" subcommand testscript.Main
// dispatches to (same os.Args[0] check testscript.Main itself makes,
// exe.go). This compiled test binary IS the "loto" testscript spawns for
// every "loto ..." line in every script, so TestMain runs again, from
// scratch, on EVERY one of those subprocesses — and each script's own Setup
// has already set LOTO_BASE (and cleared XDG_STATE_HOME) explicitly for
// that subprocess's cmd.Env, deliberately, per-script. An unconditional
// clear here would win over that (LOTO_BASE is StateDir's highest-priority
// var) and silently reset it to empty on every single "loto ..." call,
// which then falls through to xdgStateHome()+HOME — and HOME here is a
// FRESH os.MkdirTemp result on every invocation, so a script's own
// `loto lock` and its later `loto check` would land in two different,
// randomly-named state dirs instead of the one Setup meant them to share.
// That silently broke the peer-lock advisory in every git-hook-driven
// script (guard_postcheckout_moved and siblings) — caught only by running
// the full suite, not this file's own canaries, which is why it is spelled
// out here at length (loto-reuo).
func TestMain(m *testing.M) {
	if filepath.Base(os.Args[0]) != "loto" {
		fallback, err := os.MkdirTemp("", "loto-cmd-testhome-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "TestMain: mkdir fallback HOME:", err)
			os.Exit(1)
		}
		if err := os.Setenv("HOME", fallback); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain: set fallback HOME:", err)
			os.Exit(1)
		}
		if err := os.Setenv("XDG_STATE_HOME", ""); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain: clear XDG_STATE_HOME:", err)
			os.Exit(1)
		}
		if err := os.Setenv("LOTO_BASE", ""); err != nil {
			fmt.Fprintln(os.Stderr, "TestMain: clear LOTO_BASE:", err)
			os.Exit(1)
		}
		// No deferred os.RemoveAll(fallback) here — unlike the sibling floors
		// in internal/cli, internal/render and internal/identity, testscript.Main
		// below always calls os.Exit itself and never returns to this stack
		// frame, so a defer would never run. fallback is a short-lived OS temp
		// dir, not the real state dir this floor exists to protect — the OS
		// reaps it same as any other stray /tmp entry.
	}

	testscript.Main(m, map[string]func(){
		"loto": func() {
			ctx, stop := context.WithCancel(context.Background())
			defer stop()
			os.Exit(cli.RunContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
		},
	})
}

// TestHomeGuardCanary_HomeIsNeverTheRealOne is the regression test for
// TestMain's floor above: it makes no HOME redirect of its own, so it only
// passes because TestMain already repointed HOME before this — or any
// other — test in the package got to run. Delete or weaken TestMain's
// os.Setenv("HOME", ...) and this goes red immediately, instead of the leak
// silently resuming.
func TestHomeGuardCanary_HomeIsNeverTheRealOne(t *testing.T) {
	if realHomeAtProcessStart == "" {
		t.Skip("no real $HOME in this environment to compare against")
	}
	if got := os.Getenv("HOME"); got == realHomeAtProcessStart {
		t.Fatalf("HOME = %q, want anything but the real invoking-user home %q — the loto-bt6c isolation floor is not active in cmd/loto", got, realHomeAtProcessStart)
	}
}

// TestHomeGuardCanary_XDGStateHomeNeverLeaksTheRealStateDir is the
// regression test for the second half of TestMain's floor (cubic P2, PR
// #384): cli.StateDir's xdgStateHome (internal/cli/paths.go) prefers
// $XDG_STATE_HOME over $HOME and only consults the home dir when it is
// unset, so redirecting HOME alone did nothing when the invoking shell (or
// CI) exports XDG_STATE_HOME — a future plain TestXxx calling into
// internal/cli directly still resolved StateDir into whatever real directory
// XDG_STATE_HOME named, and the original canary stayed green because it only
// ever compared $HOME. LOTO_BASE overrides StateDir outright (same file), so
// the floor clears that too. Delete or weaken either clear in TestMain and
// this goes red the moment the invoking environment sets either var — e.g.
// `XDG_STATE_HOME=$HOME/.local/state go test ./cmd/loto/...`.
func TestHomeGuardCanary_XDGStateHomeNeverLeaksTheRealStateDir(t *testing.T) {
	if got := os.Getenv("XDG_STATE_HOME"); got != "" {
		t.Fatalf("XDG_STATE_HOME = %q, want empty — cli.StateDir's xdgStateHome prefers this over the redirected HOME, so a future direct internal/cli call would still land in the real state dir it names", got)
	}
	if got := os.Getenv("LOTO_BASE"); got != "" {
		t.Fatalf("LOTO_BASE = %q, want empty — cli.StateDir overrides everything with this", got)
	}
}

// readForCacheKey opens every regular file directly under dir so `go test`
// records their contents as inputs to this package's cached result. Callers
// want the side effect on the cache key, not the bytes.
func readForCacheKey(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if _, err := os.ReadFile(filepath.Join(dir, e.Name())); err != nil {
			return fmt.Errorf("read %s: %w", filepath.Join(dir, e.Name()), err)
		}
	}
	return nil
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

			// Go's test cache keys on the files the TEST BINARY opens; it
			// cannot see what a child process reads. guard_precommit.txtar
			// runs the tracked dispatcher through a real `git` subprocess, so
			// with the cache live (Makefile TEST_COUNT, loto-4ivy) an edit to
			// .githooks/pre-commit would leave a cached PASS standing until CI
			// ran it cold. Reading the directory here puts every hook's bytes
			// into this package's cache inputs, so changing one invalidates
			// the result. The contents are not otherwise used.
			if err := readForCacheKey(realHooks); err != nil {
				return err
			}

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
			// `spawnpid <ENVVAR>` — start a child that outlives the script and
			// put its pid in <ENVVAR>. A script staging a PEER session needs a
			// pid that is live and is not the test process (loto-2jgn: one
			// process is one peer, so two sessions need two pids). No constant
			// can be that: pid 1 is this process under a container that runs
			// the test binary as init, and any other number is dead or
			// recycled. The child is killed when the script ends.
			"spawnpid": func(ts *testscript.TestScript, neg bool, args []string) {
				if len(args) != 1 {
					ts.Fatalf("usage: spawnpid <ENVVAR>")
				}
				cmd := exec.Command("sleep", "300")
				if err := cmd.Start(); err != nil {
					ts.Fatalf("spawnpid: %v", err)
				}
				ts.Defer(func() {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				})
				ts.Setenv(args[0], strconv.Itoa(cmd.Process.Pid))
				if neg {
					ts.Fatalf("spawnpid unexpectedly succeeded")
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
