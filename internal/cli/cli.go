package cli

import (
	"fmt"
	"io"
)

type cmd func(args []string, stdout, stderr io.Writer) int

var registry = map[string]cmd{}

func register(name string, c cmd) { registry[name] = c }

func Run(argv []string, stdout, stderr io.Writer) int {
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
