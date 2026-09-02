package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"

	"loto/internal/identity"
)

func init() { register("whoami", cmdWhoami) } //nolint:gochecknoinits // command registry pattern

// cmdWhoami prints the owner id this process resolves to and, inside a Claude
// Code session, records the session's liveness witnesses (pid, start-time,
// messaging socket) at ~/.loto/session/<sid>.json. The SessionStart hook is
// its main caller: `loto whoami --ensure --json` feeds LOTO_AGENT_ID, and the
// record it leaves is what identity.ProbeSession answers from for every later
// stale-lock question about this session (loto-jnid).
//
// Outside a session — nothing pins an identity — it still answers, on a
// throwaway id, with a ⚠ row saying so: whoami is the one verb that must work
// from anywhere, including a bare shell asking "what would loto call me?".
func cmdWhoami(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit identity as a single JSON object (uuid/host/session)")
	// --ensure is the historical hook flag; identity.Ensure always runs, so it
	// is accepted as a no-op for back-compat with the SessionStart hook.
	_ = fs.Bool("ensure", false, "ensure an identity exists (no-op: always ensured)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	a, err := identity.Ensure(ctx)
	pinned := true
	if errors.Is(err, identity.ErrUnpinned) {
		a, err, pinned = identity.Ephemeral(), nil, false
	}
	if err != nil {
		fmt.Fprintf(stderr, "✗ identity: %v\n", err)
		return 3
	}

	// Liveness witnesses are recorded here, not in openRuntime: whoami is the
	// command the SessionStart hook already runs, so the record is written once
	// per session instead of on every lock/check/status call. A write failure
	// is a warning, never fatal — the record is evidence for OTHER sessions'
	// reclaim decisions; identity is the authority and it already resolved.
	rec, rerr := identity.RecordSession(a)
	if rerr != nil {
		fmt.Fprintf(stderr, "⚠ session not recorded: %v\n", rerr)
	}
	session := "-"
	if rec != nil {
		session = rec.SessionID
	}
	if !pinned {
		fmt.Fprintln(stderr, "⚠ identity=unpinned: this id is a throwaway; export CLAUDE_CODE_SESSION_ID or LOTO_AGENT_ID to own locks")
	}

	if *asJSON {
		// Emit the identity fields the SessionStart hook consumes. The key for
		// the owner id is "uuid" (matches identity.Agent json tags), so the hook
		// must read d["uuid"], not d["id"] (loto-u7b7).
		enc := json.NewEncoder(stdout)
		if encErr := enc.Encode(struct {
			UUID    string `json:"uuid"`
			Host    string `json:"host"`
			Session string `json:"session,omitempty"`
		}{a.UUID, a.Host, sessionOrEmpty(rec)}); encErr != nil {
			fmt.Fprintf(stderr, "✗ encode: %v\n", encErr)
			return 3
		}
		return 0
	}

	fmt.Fprintf(stdout, "uuid:    %s\nhost:    %s\nsession: %s\n", a.UUID, a.Host, session)
	return 0
}

func sessionOrEmpty(r *identity.SessionRecord) string {
	if r == nil {
		return ""
	}
	return r.SessionID
}
