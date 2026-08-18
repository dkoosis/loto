package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/render"
)

func init() { register("guard", cmdGuard) } //nolint:gochecknoinits // command registry pattern

// guardedVerbs is the set of git subcommands that move the tree under a
// peer (decision nug 682b236fdd2d / 7a2b16d6aaaf, ccp-vx4w): checkout and
// switch both repoint HEAD, restore can rewrite tracked files in place.
var guardedVerbs = map[string]bool{ //nolint:gochecknoglobals // fixed verb allowlist, not runtime state
	"checkout": true,
	"switch":   true,
	"restore":  true,
}

const guardOverrideEnv = "LOTO_GUARD_OVERRIDE"

const guardUsageHead = `usage: loto guard <checkout|switch|restore> [<git-args>...]

Repo-scoped tree-move claim (ccp-vx4w): refuses to run the wrapped git verb
while any peer holds a live lock or claim anywhere in this repo, naming the
peer. No live peer -> passes through silently. Escape hatch: set
LOTO_GUARD_OVERRIDE=1 to proceed anyway; the override is still logged.

Installed as a git alias so it fires for any session (interactive shell,
script, or agent), not just wrap flows — see 'make hooks'.

examples:
  loto guard checkout main
  LOTO_GUARD_OVERRIDE=1 loto guard switch -c feature/x
`

// cmdGuard is the wrapper `loto guard <verb> [args...]` installed as a git
// alias (make hooks) so `git checkout`/`git switch`/`git restore` resolve
// here before touching the tree. It never re-implements git — on any pass
// (no peer, override, or fail-open infra trouble) it execs the real verb via
// execRealGit, which strips the alias for that one call to avoid recursing
// into itself.
func cmdGuard(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, guardUsageHead) }
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, guardUsageHead)
		return 2
	}
	verb := fs.Arg(0)
	if !guardedVerbs[verb] {
		fmt.Fprintf(stderr, "✗ loto guard: unsupported verb %q (want checkout|switch|restore)\n", verb)
		return 2
	}
	gitArgs := fs.Args()[1:]
	override := os.Getenv(guardOverrideEnv) == "1"

	rows, failOpen := guardPeerRows(ctx, stderr)
	switch {
	case failOpen:
		// Infra/identity trouble already reported to stderr by
		// guardPeerRows; the guard never blocks on its own IO trouble
		// (same fail-open contract as `check --gate`).
	case len(rows) > 0 && !override:
		fmt.Fprintf(stderr, "✗ blocked: git %s refused — live peer holds territory in this repo\n", verb)
		render.EmitGateDeny(stderr, rows)
		fmt.Fprintf(stderr, "override: %s=1 git %s ...\n", guardOverrideEnv, verb)
		return 1
	case len(rows) > 0 && override:
		fmt.Fprintf(stderr, "⚠ override: %s=1 — proceeding despite %d live peer claim(s)/lock(s)\n", guardOverrideEnv, len(rows))
		render.EmitGateDeny(stderr, rows)
	}
	return execRealGit(ctx, verb, gitArgs, stdout, stderr)
}

// guardPeerRows opens a runtime and evaluates gateDecideAny, mirroring
// runCheckGate's fail-open contract (gate-design "Rules: fail-open,
// everywhere"): an unpinned identity or unreachable store must never block a
// git operation, since that would turn a loto outage into a repo-wide
// checkout freeze. failOpen=true means the caller must proceed regardless of
// rows (rows is nil in that case); the fail-open reason is already written
// to stderr by this function, matching gateInfraUnreachable's stream choice
// (loto-tzmv.8 — a silent fail-open reads as protection that isn't there).
func guardPeerRows(ctx context.Context, stderr io.Writer) (rows []render.GateDenyRow, failOpen bool) {
	if !identity.PinnedByEnv() {
		fmt.Fprintln(stderr, "⚠ identity=unpinned guard=fail-open")
		return nil, true
	}
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ store=unreachable guard=fail-open err=%q\n", err)
		return nil, true
	}
	defer rt.Close()

	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ store=unreachable guard=fail-open err=%q\n", err)
		return nil, true
	}
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ store=unreachable guard=fail-open err=%q\n", err)
		return nil, true
	}
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}
	return gateDecideAny(locks, claims, rt.Agent.UUID, ec), false
}

// execRealGit runs the wrapped verb against the actual git binary. `-c
// alias.<verb>=` clears the alias that dispatched here for this one
// subprocess only — without it, a `git checkout` alias pointing at `loto
// guard checkout` would have this exec re-resolve the same alias and recurse
// until the process table or the stack gives out.
func execRealGit(ctx context.Context, verb string, gitArgs []string, stdout, stderr io.Writer) int {
	fullArgs := append([]string{"-c", "alias." + verb + "="}, verb)
	fullArgs = append(fullArgs, gitArgs...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "✗ loto guard: exec git %s: %v\n", verb, err)
		return 1
	}
	return 0
}
