// Command loto is the CLI for lock-out/tag-out coordination across concurrent
// Claude sessions. v2: SQLite-backed lock + tag store; no daemon, no flock.
package main

import (
	"os"

	"loto/internal/cli"
)

func main() {
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
