package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"loto/internal/identity"
)

func init() { register("alive", cmdAlive) } //nolint:gochecknoinits // command registry pattern

// aliveUsage is the point-of-use teaching surface (same policy as whoUsage):
// Claude is loto's primary user, and the verdict this command prints is the
// input to kill-class decisions — the rules for reading it belong in the
// binary, not in a doc a reaper's author may never open.
const aliveUsage = `usage: loto alive [<handle-or-uuid>...]

Ask the one liveness oracle whether the Claude Code session behind an agent is
still up. With no arguments, every recorded peer is checked.

  <handle>  loto handle, matched case-insensitively against the peer registry
  <uuid>    agent uuid in canonical hex form

A DEAD verdict is necessary but never sufficient for kill-class actions:
killing additionally requires an idle/TaskStop signal from the harness. LIVE
vetoes a kill. UNKNOWN means no peer record — fall back to TTL, never treat as
dead.

exit 0  nothing queried is provably dead — live and unknown both land here
exit 1  at least one queried subject is dead
exit 2  usage
exit 3  the peer registry could not be read

Callers gating kill-class actions must therefore read exit 0 as "not provably
dead", never as "alive".

examples:
  loto alive NeatDace
  loto alive NeatDace GoldBadger && echo "no proof either is gone — do not kill"`

// uuidShape mirrors identity's canonical agent-uuid layout. Duplicated rather
// than exported: it decides display only (which column an unresolved argument
// belongs in), never access to a path.
var uuidShape = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`) //nolint:gochecknoglobals // compiled once, read-only

// aliveRow is one printed subject: the identity asked about plus the oracle's
// answer, held together so counts and rows cannot disagree.
type aliveRow struct {
	handle  string
	uuid    string
	verdict identity.LivenessVerdict
}

func cmdAlive(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("alive", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, aliveUsage) }
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}

	// No openRuntime: peer presence is host-global identity state, so `alive`
	// answers outside a git repo — the same reach as `who` and `whoami`.
	// includeDead: a dead peer is the whole point of asking, and the record is
	// the last observation of a session that has already gone.
	peers, err := identity.Peers(ctx, true)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}

	return emitAlive(stdout, aliveRows(ctx, peers, fs.Args()))
}

// aliveRows resolves the asked-for subjects to verdicts. Rows come from the
// listing already in hand rather than a fresh per-argument read: Peers unlinks
// dead records as it lists them, so re-reading by uuid would turn the dead
// verdict this command exists to report into no-peer-record.
func aliveRows(ctx context.Context, peers []identity.Peer, args []string) []aliveRow {
	rows := make([]aliveRow, 0, max(len(peers), len(args)))
	if len(args) == 0 {
		for i := range peers {
			rows = append(rows, peerRow(ctx, &peers[i]))
		}
		return sortAliveRows(rows)
	}
	for _, arg := range args {
		matched := false
		for i := range peers {
			if strings.EqualFold(peers[i].Handle, arg) || strings.EqualFold(peers[i].UUID, arg) {
				rows = append(rows, peerRow(ctx, &peers[i]))
				matched = true
			}
		}
		if !matched {
			rows = append(rows, unresolvedRow(ctx, arg))
		}
	}
	return sortAliveRows(rows)
}

func peerRow(ctx context.Context, p *identity.Peer) aliveRow {
	return aliveRow{
		handle:  dashIfEmpty(p.Handle),
		uuid:    dashIfEmpty(p.UUID),
		verdict: p.SessionVerdict(ctx),
	}
}

// unresolvedRow answers for a subject the listing did not cover. AgentLive is
// still the door: it re-reads the registry, so a record written between the
// listing and now is honored instead of reported absent.
func unresolvedRow(ctx context.Context, arg string) aliveRow {
	v := identity.AgentLive(ctx, arg)
	if v.Peer != nil {
		return aliveRow{handle: dashIfEmpty(v.Peer.Handle), uuid: dashIfEmpty(v.Peer.UUID), verdict: v}
	}
	if uuidShape.MatchString(arg) {
		return aliveRow{handle: "-", uuid: arg, verdict: v}
	}
	return aliveRow{handle: arg, uuid: "-", verdict: v}
}

// sortAliveRows makes the output reproducible for the same input (design.md):
// handle first because that is what a caller asked with, uuid as the tiebreak
// so the order is total even across same-handle records.
func sortAliveRows(rows []aliveRow) []aliveRow {
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].handle != rows[j].handle {
			return rows[i].handle < rows[j].handle
		}
		return rows[i].uuid < rows[j].uuid
	})
	return rows
}

// emitAlive prints the triage counts, then one row per subject, and returns the
// exit code. Only a DEAD verdict fails: unknown is an absence of evidence, and
// exiting non-zero on it would license exactly the kill the oracle exists to
// prevent.
func emitAlive(w io.Writer, rows []aliveRow) int {
	var live, dead, unknown int
	for i := range rows {
		switch rows[i].verdict.Liveness {
		case identity.SessionLive:
			live++
		case identity.SessionDead:
			dead++
		case identity.SessionUnknown:
			unknown++
		}
	}
	// Explicit even at zero: silence reads as a crash (design.md).
	fmt.Fprintf(w, "ℹ checked=%d live=%d dead=%d unknown=%d\n", len(rows), live, dead, unknown)
	for i := range rows {
		r := &rows[i]
		fmt.Fprintf(w, "%s agent=%s uuid=%s liveness=%s reason=%s\n",
			aliveGlyph(r.verdict.Liveness), r.handle, r.uuid, r.verdict.Liveness, r.verdict.Reason)
	}
	if dead > 0 {
		return 1
	}
	return 0
}

func aliveGlyph(s identity.SessionLiveness) string {
	switch s {
	case identity.SessionLive:
		return glyphPass
	case identity.SessionDead:
		return glyphFail
	case identity.SessionUnknown:
		return "⚠"
	default:
		return "⚠"
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
