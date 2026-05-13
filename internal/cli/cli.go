package cli

import (
	"context"
	"fmt"
	"io"
)

type cmd func(args []string, stdout, stderr io.Writer) int

var registry = map[string]cmd{}

// rootCtx holds the SIGINT-aware context provided by Run. Commands read it via
// runtimeCtx() so signal cancellation propagates into git/exec callsites without
// threading ctx through every cmd_*.go signature.
var rootCtx context.Context = context.Background() //nolint:gochecknoglobals // request-scope ctx set once by Run

func register(name string, c cmd) { registry[name] = c }

func runtimeCtx() context.Context { return rootCtx }

func Run(argv []string, stdout, stderr io.Writer) int {
	return RunContext(context.Background(), argv, stdout, stderr)
}

func RunContext(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if ctx != nil {
		rootCtx = ctx //nolint:fatcontext // single CLI invocation per process — set once at startup
	}
	if len(argv) == 0 {
		printHelp(stderr)
		return 2
	}
	if c, ok := registry[argv[0]]; ok {
		return c(argv[1:], stdout, stderr)
	}
	// Bare form: loto <target> -t "note" — annotate without locking.
	// argv[0] is the target path (not a known command).
	return cmdAnnotate(argv, stdout, stderr)
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `usage: loto <command> [args]

commands:
  lock     Acquire a lock on one or more targets; -t required
  unlock   Release locks; --force to break another's, --all to release all yours
  msg      Send or read agent-to-agent messages
  tag      Attach a note to a target; --to to address an agent
  untag    Remove a tag from a target
  check    Check targets for lock conflicts; --staged reads git staged paths
  status   Show lock state; --mine to filter to yours
  doctor   Detect and optionally repair stale locks
  whoami   Print agent identity
  version  Print loto version

bare form:
  loto <target> -t "note"   Annotate without locking (same as tag)`)
}
