package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"loto/internal/identity"
)

func init() { register("who", cmdWho) } //nolint:gochecknoinits // command registry pattern

// whoUsage is the point-of-use teaching surface (same policy as tagUsage):
// Claude is loto's primary user, so the input contract lives in the binary.
const whoUsage = `usage: loto who [<handle>] [--json] [--all]

Print the handle ↔ Claude Code peer-name table for sessions live on this host.
Answers the question you have right before SendMessage: "I know this agent as
LotoHandle — what name does the messaging layer know it by?"

  <handle>  print just that agent's row; exit 1 if it has no live peer
  --json    emit the rows as a JSON array
  --all     include peers whose session is gone (last observation)

Liveness is socket-file existence, not process liveness — a worktree agent
reads as dead to the lock-liveness check (loto-r11w) but is plainly reachable
here. Dead rows are pruned from disk as they are listed.

A row with peer=- is a top-level session: Claude Code names those from their
cwd or /rename, and that name is readable nowhere a loto hook can reach. Make
it addressable by running /rename <handle> in that session, or have it record
its own name with 'loto whoami --peer-name <name>'.`

func cmdWho(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("who", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, whoUsage) }
	asJSON := fs.Bool("json", false, "emit the rows as a JSON array")
	all := fs.Bool("all", false, "include peers whose session is gone")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, whoUsage)
		return 2
	}

	// No openRuntime: peer presence is host-global identity state, so `who`
	// answers outside a git repo — the same reach as `whoami`.
	peers, err := identity.Peers(*all)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	want := fs.Arg(0)
	if want != "" {
		peers = filterPeersByHandle(peers, want)
		if len(peers) == 0 {
			fmt.Fprintf(stderr, "✗ no live peer for handle %q (try: loto who --all)\n", want)
			return 1
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(peers); encErr != nil {
			fmt.Fprintf(stderr, "✗ encode: %v\n", encErr)
			return 3
		}
		return 0
	}
	emitPeers(stdout, peers, time.Now())
	return 0
}

func filterPeersByHandle(peers []identity.Peer, handle string) []identity.Peer {
	out := peers[:0:0]
	for i := range peers {
		if strings.EqualFold(peers[i].Handle, handle) {
			out = append(out, peers[i])
		}
	}
	return out
}

// emitPeers renders the table: count on the first body line, one key=value row
// per peer, no ANSI (docs/design.md). The unnamed footer fires once, not per
// row — the remedy is the same for every one of them.
func emitPeers(w io.Writer, peers []identity.Peer, now time.Time) {
	fmt.Fprintf(w, "peers: %d\n", len(peers))
	unnamed := 0
	for i := range peers {
		p := &peers[i]
		name := p.Name
		if name == "" {
			name = "-"
			unnamed++
		}
		state := "live"
		if !p.Live() {
			state = "gone"
		}
		fmt.Fprintf(w, "handle=%s peer=%s state=%s pid=%d cwd=%s socket=%s seen=%s\n",
			p.Handle, name, state, p.PID, tildeHome(p.CWD), p.Socket, humanAge(now.Sub(p.SeenAt)))
	}
	if unnamed > 0 {
		fmt.Fprintf(w, "∇ %d peer=- (unnamed): SendMessage needs a name — run /rename <handle> in that session\n", unnamed)
	}
}

// tildeHome shortens a home-anchored path for display only; the JSON output
// keeps the absolute path.
func tildeHome(p string) string {
	if p == "" {
		return "-"
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	rel, err := filepath.Rel(home, p)
	if err != nil || !filepath.IsLocal(rel) {
		return p
	}
	return filepath.Join("~", rel)
}

// humanAge renders an elapsed duration at one unit of precision. Ages are
// glanceable context ("seen 3m ago"), never a value anything computes on.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
