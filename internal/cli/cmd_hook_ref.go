package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

func init() { register("hook", cmdHook) } //nolint:gochecknoinits // command registry pattern

// ── `loto hook ref` — I1, the tree-move refusal ───────────────────────────
//
// git's `reference-transaction` hook is the one chokepoint every H1 member
// crosses: a branch switch, a stash, a branch deletion and a worktree branch
// all reach it, and a hook exiting non-zero in the `prepared` phase makes git
// abort the whole transaction atomically (enforcement-design §2.4, measured
// on git 2.55.0). Nothing here reads a command string: git hands over no
// operation name, and the decision is on (old, new, ref) and the live-session
// set alone (§5 I1).
//
// The refusal fires only while a peer could be hurt — two or more live
// sessions in this checkout — and the operator's override is a checkout-wide
// claim, `loto claim .`, which is a durable statement of "the whole tree is
// mine right now" rather than an env-var escape nobody can audit.

// The three protected shapes, spelled as the reason a refusal event carries.
const (
	refShapeHeadSymref   = "head-symref"
	refShapeStash        = "stash"
	refShapeBranchDelete = "branch-delete"
)

// refHeadsPrefix / refStash / refHEAD are the ref names I1 is defined over.
const (
	refHeadsPrefix = "refs/heads/"
	refStash       = "refs/stash"
	refHEAD        = "HEAD"
)

// refSymrefPrefix is how git spells a symbolic-ref value in the transaction
// it hands the hook: `0000… ref:refs/heads/other HEAD` for `git checkout
// other`. Measured on git 2.55.0.
const refSymrefPrefix = "ref:"

// refOverrideWindow is how long after a refusal a checkout-wide claim by the
// same owner still counts as an override of it (§10b row 1). Ten minutes is
// the spec's number: long enough for a session to read the refusal, decide
// the guard was wrong and take the claim; short enough that a claim taken for
// an unrelated reason an hour later is not miscounted as an override.
const refOverrideWindow = 10 * time.Minute

// refCheckoutWidePrefix is the claim prefix that overrides I1 — the repo
// root, which CanonicalizePrefix spells ".". `loto claim . -t "..."` already
// documents itself as a takeover of the whole checkout.
const refCheckoutWidePrefix = "."

// hookUsageHead is the teaching surface for the hook router.
const hookUsageHead = `usage: loto hook ref <phase>

Git hook bodies. Not called by hand — each is wired as a chain entry under
.githooks/hooks.d/<hook>/ and reaches git through core.hooksPath (make hooks).

  ref <phase>   reference-transaction body (I1). Reads git's
                "<old> <new> <ref>" lines on stdin and refuses, at the
                'prepared' phase, a protected ref shape issued while two or
                more sessions are live in this checkout:

                  HEAD changing its symbolic target   (branch switch)
                  any update to refs/stash            (stash, stash pop)
                  a branch deletion under refs/heads/

                Everything else passes: a branch tip moving, refs/remotes
                pruning, pack-refs, reflog expiry.

                Override: take the checkout-wide claim.

exit: 0 pass · 1 refused (git aborts the transaction) · 2 usage · 3 fail-open

examples:
  loto hook ref prepared < transaction.txt
`

func cmdHook(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}
	switch args[0] {
	case subHelp, "-h", flagHelpLong:
		fmt.Fprint(stderr, hookUsageHead)
		return 0
	case "ref":
		return cmdHookRef(ctx, args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "✗ unknown hook body: %s\n", args[0])
	fmt.Fprint(stderr, hookUsageHead)
	return 2
}

func cmdHookRef(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}
	if args[0] == subHelp || args[0] == "-h" || args[0] == flagHelpLong {
		fmt.Fprint(stderr, hookUsageHead)
		return 0
	}
	return runHookRef(ctx, args[0], os.Stdin, stdout, stderr)
}

// refUpdate is one line of git's reference-transaction stdin.
type refUpdate struct {
	Old string
	New string
	Ref string
}

// refRefusal binds a refused update to the shape that refused it.
type refRefusal struct {
	Update refUpdate
	Shape  string
}

// refZeroOID reports whether an object id is git's all-zeros null oid. Length
// is not pinned: sha1 repos spell it 40 zeros, sha256 repos 64.
func refZeroOID(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// classifyRefUpdate names the protected shape u matches, or "" if I1 admits
// it. The whole of `allow` from §5 lives here, and it is a three-shape
// DENYLIST, never deny-by-default: a shape nobody listed passes, so repo
// maintenance is not starved by a guard that has never heard of it.
//
// ‡ The branch-delete arm requires BOTH oids null, and that is a measurement,
// not a typo. On git 2.55.0 every route to deleting a branch — `git branch
// -D` on a loose ref, on a packed ref, and `git update-ref -d <ref> <oldoid>`
// with the old value stated — reports the update as `0000… 0000… refs/heads/x`.
// `git pack-refs --all` also emits `new=0` lines under refs/heads/, one per
// ref, as it removes each loose file AFTER the packed-refs file already holds
// it: those carry the real old oid, `<sha> 0000… refs/heads/x`. Refusing on
// `new=0` alone therefore breaks `git pack-refs` and `git gc` (measured: exit
// 128, "failed to run pack-refs") — which §5 forbids in the same sentence
// that defines the shape. The old-oid test is the only discriminator
// available to a hook that is given no operation name.
func classifyRefUpdate(u refUpdate) string {
	switch {
	case u.Ref == refHEAD && strings.HasPrefix(u.New, refSymrefPrefix):
		return refShapeHeadSymref
	case u.Ref == refStash:
		return refShapeStash
	case strings.HasPrefix(u.Ref, refHeadsPrefix) && refZeroOID(u.New) && refZeroOID(u.Old):
		return refShapeBranchDelete
	}
	return ""
}

// parseRefTransaction reads git's "<old> <new> <ref>" lines. A ref name may
// not contain a space (git's own check-ref-format rule), so cutting on the
// first two spaces is exact rather than approximate — and it keeps a symref
// value like "ref:refs/heads/other" intact in the middle field.
//
// A malformed line is skipped rather than fatal: this reader's job is to find
// protected shapes, and a line it cannot parse is a line it cannot claim to
// have judged. Every failure direction here is "pass", which is the fail-open
// stance the rest of loto's gates take.
func parseRefTransaction(r io.Reader) []refUpdate {
	var out []refUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		oldOID, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		newOID, ref, ok := strings.Cut(rest, " ")
		if !ok || ref == "" {
			continue
		}
		out = append(out, refUpdate{Old: oldOID, New: newOID, Ref: ref})
	}
	return out
}

// runHookRef is the IO runner. Order of the checks is a cost decision as much
// as a logic one: the shape test is pure string work over a handful of lines,
// so the overwhelmingly common transaction — a commit moving a branch tip —
// exits without reading the session directory or opening the store at all.
func runHookRef(ctx context.Context, phase string, stdin io.Reader, stdout, stderr io.Writer) int {
	updates := parseRefTransaction(stdin)

	// Only `prepared` can refuse: git aborts the transaction there and nothing
	// has been written. `committed` and `aborted` are after the fact, and
	// `preparing` has not resolved symrefs yet.
	if phase != "prepared" {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d phase=%s\n", len(updates), phase)
		return 0
	}

	refusals := make([]refRefusal, 0, len(updates))
	for _, u := range updates {
		if shape := classifyRefUpdate(u); shape != "" {
			refusals = append(refusals, refRefusal{Update: u, Shape: shape})
		}
	}
	if len(refusals) == 0 {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d\n", len(updates))
		return 0
	}

	// A hook with no session id is not in S and claims no protection: a bare
	// shell, a cron job, a human's terminal. It passes (§5 I1).
	if identity.SessionIDFromEnv() == "" {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d session=absent\n", len(updates))
		return 0
	}

	// S — live sessions, from session records rather than call records, so
	// two idle peers still protect each other (§3, spec test 13).
	live := identity.LiveOwnerUUIDs()
	if len(live) < 2 {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d\n", len(updates), len(live))
		return 0
	}

	rt, err := openRuntime(ctx)
	if err != nil {
		// Fail-open, loudly. Without the store the override claim cannot be
		// read, and refusing every branch switch because loto's own database
		// is unreachable is the gate becoming the outage.
		fmt.Fprintf(stderr, "⚠ store=unreachable ref-guard=fail-open err=%q\n", err)
		return 3
	}
	defer rt.Close()

	if held, holder := refCheckoutWideClaim(rt); held {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d override=claim holder=%s\n",
			len(updates), len(live), holder)
		return 0
	}

	recordRefRefusals(rt, refusals, len(live), stderr)
	printRefRefusal(stderr, refusals, rt.Agent.UUID, live)
	return 1
}

// refCheckoutWideClaim reports whether this caller holds the live
// checkout-wide claim. Kin counts: a subagent stamped by the dispatch hook
// resolves to its parent's owner (§3), so a lane's fan-out is covered by the
// claim the lane took.
func refCheckoutWideClaim(rt *runtime) (bool, string) {
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		return false, ""
	}
	mine := map[string]struct{}{rt.Agent.UUID: {}}
	if kin, kerr := parentKin(rt.Ctx); kerr == nil {
		for _, k := range kin {
			mine[string(k)] = struct{}{}
		}
	}
	now := time.Now()
	for i := range claims {
		c := claims[i]
		if c.PathPrefix != refCheckoutWidePrefix || c.Expired(now) {
			continue
		}
		if _, ok := mine[string(c.OwnerUUID)]; ok {
			return true, string(c.OwnerUUID)
		}
	}
	return false, ""
}

// refRefusedDetail is the §10b row-1 payload: the transaction git offered and
// how many sessions were live when it was refused.
type refRefusedDetail struct {
	Old  string `json:"old"`
	New  string `json:"new"`
	Ref  string `json:"ref"`
	Live int    `json:"live"`
}

// recordRefRefusals writes one ref_refused event per refused update. Failing
// to record is reported and never changes the verdict: a lost counter row
// costs the telemetry a data point, and letting it undo the refusal would
// cost a peer their working tree.
func recordRefRefusals(rt *runtime, refusals []refRefusal, live int, stderr io.Writer) {
	now := time.Now()
	for i := range refusals {
		r := refusals[i]
		detail, err := json.Marshal(refRefusedDetail{
			Old: r.Update.Old, New: r.Update.New, Ref: r.Update.Ref, Live: live,
		})
		if err != nil {
			detail = nil
		}
		// ‡ AppendEventRotating, not AppendEvent: this fires on ref
		// transactions in a busy shared checkout and nothing else on the path
		// trims the table.
		if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
			Kind:      store.EventRefRefused,
			Target:    domain.Target{Canonical: r.Update.Ref},
			ActorUUID: rt.Agent.UUID,
			Reason:    r.Shape,
			Detail:    string(detail),
			CreatedAt: now,
		}); err != nil {
			fmt.Fprintf(stderr, "⚠ ref-refused-counter=unrecorded err=%q\n", err)
		}
	}
}

// printRefRefusal renders the refusal. It goes to stderr because that is what
// git relays to whoever ran the command, and it names three things the reader
// needs: which shape was refused, who else is live, and the exact override.
func printRefRefusal(stderr io.Writer, refusals []refRefusal, self string, live map[string]struct{}) {
	fmt.Fprintf(stderr, "✗ ref-refused count=%d live=%d\n", len(refusals), len(live))
	rows := make([]string, 0, len(refusals))
	for i := range refusals {
		r := refusals[i]
		rows = append(rows, fmt.Sprintf("✗ ref=%s shape=%s old=%s new=%s",
			r.Update.Ref, r.Shape, r.Update.Old, r.Update.New))
	}
	sort.Strings(rows)
	for _, row := range rows {
		fmt.Fprintln(stderr, row)
	}
	peers := make([]string, 0, len(live))
	for uuid := range live {
		if uuid != self {
			peers = append(peers, uuid)
		}
	}
	sort.Strings(peers)
	fmt.Fprintf(stderr, "ℹ live-peers=%s\n", strings.Join(peers, ","))
	fmt.Fprintln(stderr, "ℹ this move would rewrite their working tree; a stash or branch delete is not undoable for them")
	fmt.Fprintln(stderr, "ℹ override=checkout-wide-claim")
	fmt.Fprintln(stderr, "```bash")
	fmt.Fprintln(stderr, `loto claim . -t "<reason>"`)
	fmt.Fprintln(stderr, "```")
}
