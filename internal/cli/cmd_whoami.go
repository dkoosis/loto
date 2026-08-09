package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"loto/internal/identity"
)

func init() { register("whoami", cmdWhoami) } //nolint:gochecknoinits // command registry pattern

func cmdWhoami(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit identity as a single JSON object (uuid/handle/host/peer)")
	peerName := fs.String("peer-name", "", "record this session's Claude Code peer name (its SendMessage address)")
	// --ensure is the historical hook flag; identity.Ensure always runs, so it
	// is accepted as a no-op for back-compat with the SessionStart hook.
	_ = fs.Bool("ensure", false, "ensure an identity exists (no-op: always ensured)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	a, err := identity.Ensure(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ identity: %v\n", err)
		return 3
	}

	// Presence is recorded here, not in openRuntime: whoami is the command the
	// SessionStart hook already runs, so the peer table is written once per
	// session instead of shelling out to `ps` on every lock/check/status call.
	// A write failure is a warning, never fatal — presence is a convenience
	// table; identity is the authority and it already resolved.
	peer, perr := identity.RecordPeer(ctx, a, *peerName)
	if perr != nil {
		fmt.Fprintf(stderr, "∇ peer not recorded: %v\n", perr)
	}
	peerLabel := "-"
	if peer != nil && peer.Named() {
		peerLabel = peer.Name
	}

	if *asJSON {
		// Emit the identity fields the SessionStart hook consumes, plus the
		// recorded peer name (omitted when this session has none). The key for
		// the agent id is "uuid" (matches identity.Agent json tags), so the
		// hook must read d["uuid"], not d["id"] (loto-u7b7).
		enc := json.NewEncoder(stdout)
		if encErr := enc.Encode(struct {
			UUID   string `json:"uuid"`
			Handle string `json:"handle"`
			Host   string `json:"host"`
			Peer   string `json:"peer,omitempty"`
		}{a.UUID, a.Handle, a.Host, peerNameOrEmpty(peer)}); encErr != nil {
			fmt.Fprintf(stderr, "✗ encode: %v\n", encErr)
			return 3
		}
		return 0
	}

	fmt.Fprintf(stdout, "handle: %s\nuuid:   %s\nhost:   %s\npeer:   %s\n", a.Handle, a.UUID, a.Host, peerLabel)
	if peer != nil && !peer.Named() {
		fmt.Fprintln(stdout, "∇ peer name not derivable here: run /rename "+a.Handle+" in this session, or loto whoami --peer-name <name>")
	}
	return 0
}

func peerNameOrEmpty(p *identity.Peer) string {
	if p == nil {
		return ""
	}
	return p.Name
}
