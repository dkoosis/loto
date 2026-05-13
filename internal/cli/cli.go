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
	fmt.Fprintf(stderr, "unknown command: %s\n", argv[0])
	printHelp(stderr)
	return 2
}

func printHelp(w io.Writer) {
	fmt.Fprintln(w, `usage: loto <command> [args]

commands:
  lock     Acquire a lock on one or more targets; -t required
  unlock   Release locks; --force to break another's, --all to release all yours
  check    Check targets for lock conflicts; --staged reads git staged paths
  status   Show lock state; --mine to filter to yours
  doctor   Detect and optionally repair stale locks
  whoami   Print agent identity
  version  Print loto version`)
}
