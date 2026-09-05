package cli

import (
	"context"
	"fmt"
	"io"
)

// lockStaleReason tags a blocked target whose lock/claim liveness expired
// (shared between "loto lane" and "loto submit" blocked-target reporting).
const lockStaleReason = "lock-stale"

// flagHelpLong / subHelp are the two spellings of "show me the usage" every
// subcommand router matches, named once so the routers stay comparable.
const (
	flagHelpLong = "--help"
	subHelp      = "help"
)

type cmd func(ctx context.Context, args []string, stdout, stderr io.Writer) int

var registry = map[string]cmd{} //nolint:gochecknoglobals // command registry pattern

func register(name string, c cmd) { registry[name] = c }

func RunContext(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(argv) == 0 {
		printHelp(stderr)
		return 2
	}
	switch argv[0] {
	case "help", "-h", "--help":
		printHelp(stdout)
		return 0
	}
	if c, ok := registry[argv[0]]; ok {
		return c(ctx, argv[1:], stdout, stderr)
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
  refresh  Extend the TTL on locks you hold; --all for every lock of yours
  claim    Reserve a path-prefix territory for this session; -t required
  unclaim  Release your claim by path-prefix
  check    Check targets for lock conflicts; --staged reads git staged paths, --branch guards publication
  guard    Wrap checkout|switch|restore; refuses under a live peer (make hooks installs it)
  status   Show lock state; --mine to filter to yours, --collisions for ≥2-owner targets
  doctor   Detect and optionally repair stale locks
  sync     Fast-forward unleased paths from refs/loto/integration; report conflicts
  tag      Leave a note on a locked target, or pin one to unlocked territory
  violations  List, scan for, or resolve unauthorized writes to unleased paths
  ack      Dismiss a tag or territory tag by ID
  whoami   Print this session's owner id and record its liveness witnesses
  version  Print loto version

lane choreography (engine verbs; used by the /team fleet harness):
  lane     Commit an exact write-set to a lane ref by plumbing; needs every write-set lock held
  verify   Run a command against a commit in a throwaway worktree; exit 1 fail, 3 infra
  beacon   Mint a short-TTL shared lease on paths this agent is about to write (PreToolUse gate)
  submit   Package held-lock edits into a git-gate candidate: commit, capture, admit
  gate     stats — admission verdicts per rejection class over a window
  promote  Drain accepted candidates onto refs/loto/integration; verify runs unlocked
  pr       Bridge promoted integration commits to GitHub: one branch and one PR per bead`)
}
