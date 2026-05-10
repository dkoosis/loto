package cli

import (
	"fmt"
	"io"
	"sort"
)

type cmd func(args []string, stdout, stderr io.Writer) int

var registry = map[string]cmd{}

func register(name string, c cmd) { registry[name] = c }

func Run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprintln(stderr, "usage: loto <command> [args]")
		printCommands(stderr)
		return 2
	}
	c, ok := registry[argv[0]]
	if !ok {
		fmt.Fprintf(stderr, "unknown command: %s\n", argv[0])
		printCommands(stderr)
		return 2
	}
	return c(argv[1:], stdout, stderr)
}

func printCommands(w io.Writer) {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintln(w, "commands:")
	for _, n := range names {
		fmt.Fprintf(w, "  %s\n", n)
	}
}
