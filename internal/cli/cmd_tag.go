package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

func init() { register("tag", cmdTag) } //nolint:gochecknoinits // command registry pattern

// tagUsage is the point-of-use teaching surface (loto-5rwc): Claude is loto's
// primary user, so the input contract lives in the binary, not a drift-prone
// skill. Convention: open the text with the requester's bead id, then a
// <=3-word ask. The bead id resolves epic/gh-issue via beads metadata — do not
// duplicate those here.
const tagUsage = `usage: loto tag <file-or-prefix> <text...>

Leave a note on a target locked by another agent, or — when nobody holds it —
pin one to the territory for whoever arrives there next.

Which one you get is decided by the ground, not a flag: a locked target takes
today's per-holder tag; an unlocked path or a directory prefix becomes a
territory tag with a TTL (72h default, 30d max, --ttl to change). The two
confirmations read differently, so a mistyped path shows up immediately.

Convention: open the text with your bead id, then a <=3-word ask.
The bead id resolves epic/gh-issue via beads metadata — don't duplicate them.

examples:
  loto tag internal/store/store.go loto-c6rg: want next
  loto tag a.go loto-5rwc: ETA?
  loto tag internal/store loto-abc: rebase before you touch claims.go`

// beadIDPrefix matches a leading "<prefix>-<slug>:" bead reference, e.g.
// "loto-c6rg:". Used for light input shaping only — a miss WARNs, never rejects.
var beadIDPrefix = regexp.MustCompile(`^[a-z][a-z0-9]*-[a-z0-9]+:`)

// territoryTagTTL is how long a note waits on unheld ground for its reader.
//
// ‡ A note's life is bounded by when someone next VISITS the territory, not by
// a session. The lock TTL (30m) and the claim TTL (2h) are session-scale and
// would drop a note before the next agent starts work. 72h clears a weekend,
// which is the gap this fleet actually leaves on a repo. A week was rejected:
// a week-old note's premises — bead status, branch, whether the work already
// landed — are usually wrong by then, and a confidently stale note mis-teaches
// the next agent worse than silence does.
const territoryTagTTL = 72 * time.Hour

func cmdTag(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("tag", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintln(stderr, tagUsage) }
	ttl := fs.Duration("ttl", territoryTagTTL, "territory tag lifetime (unheld targets only; 30d max)")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	ttlSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "ttl" {
			ttlSet = true
		}
	})
	if fs.NArg() < 2 {
		fmt.Fprintln(stderr, tagUsage)
		return 2
	}
	text := strings.TrimSpace(strings.Join(fs.Args()[1:], " "))
	if text == "" {
		fmt.Fprintln(stderr, "✗ tag text required")
		return 2
	}
	if *ttl <= 0 {
		fmt.Fprintf(stderr, "✗ --ttl must be positive, got %s\n", *ttl)
		return 2
	}
	warnIfNoBeadID(text, stderr)
	repoTop, _ := repoTopForCwd(ctx)

	// A directory is not a lockable target, so resolveCLITarget rejects it
	// before any store call. That rejection is the ONLY thing loosened here:
	// a directory can never be locked, so falling back to prefix
	// canonicalization cannot change what happens to a file argument.
	canonical, prefixOnly, rc := resolveTagGround(repoTop, fs.Arg(0), stderr)
	if rc != 0 {
		return rc
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	if prefixOnly {
		return runTerritoryTag(rt, canonical, text, *ttl, stdout, stderr)
	}
	return runTag(rt, canonical, text, *ttl, ttlSet, stdout, stderr)
}

// resolveTagGround canonicalizes the tag's first argument. prefixOnly is true
// when the argument named a directory, which can only ever be territory.
//
// A glob gets the existing ErrTargetIsGlob rejection plus the fix block: loto's
// territory vocabulary is the bare prefix (`loto claim internal/store`), and a
// caller reaching for `internal/store/**` has the right idea in the wrong
// spelling.
func resolveTagGround(repoTop, raw string, stderr io.Writer) (canonical string, prefixOnly bool, rc int) {
	target, err := resolveCLITarget(repoTop, raw)
	if err == nil {
		return target.Canonical, false, 0
	}
	if errors.Is(err, domain.ErrTargetIsDir) {
		prefix, perr := resolveCLIPrefix(repoTop, raw)
		if perr != nil {
			fmt.Fprintf(stderr, "✗ %v\n", perr)
			return "", false, 2
		}
		return prefix.Canonical, true, 0
	}
	fmt.Fprintf(stderr, "✗ %v\n", err)
	if errors.Is(err, domain.ErrTargetIsGlob) {
		fmt.Fprintf(stderr, "```bash\nloto tag %s <text...>\n```\n", strings.TrimSuffix(strings.TrimSuffix(raw, "*"), "/"))
	}
	return "", false, 2
}

// warnIfNoBeadID shapes input lightly: if the text does not open with a
// "<bead-id>:" prefix, warn but proceed. Agents aren't always under a bead and
// humans tag too, so the free-text field stays — this is a nudge, not a gate.
//
// It fires at most once per session (loto-2hl0): a fleet's coordination
// traffic is dozens of `loto tag` calls, and a nudge repeated on every one of
// them stops teaching and starts training the reader to skip stderr.
func warnIfNoBeadID(text string, stderr io.Writer) {
	if beadIDPrefix.MatchString(text) {
		return
	}
	if !claimBeadWarn() {
		return
	}
	fmt.Fprintln(stderr, "∇ tag text should open with your bead id (e.g. loto-c6rg: want next)")
}

// beadWarnMarker is the once-per-session witness file. Var so tests can point
// it at a temp dir instead of the real one.
var beadWarnMarker = defaultBeadWarnMarker //nolint:gochecknoglobals // test seam for the once-per-session marker

func defaultBeadWarnMarker() (string, bool) {
	sid := os.Getenv("LOTO_SESSION_ID")
	if sid == "" {
		sid = os.Getenv("CLAUDE_CODE_SESSION_ID")
	}
	// No session id (a bare human shell, one command at a time) → no marker,
	// so the nudge stays on. Reject any path-shaped id rather than sanitizing:
	// the fallback is simply "warn every time", which is the old behavior.
	if sid == "" || strings.ContainsAny(sid, `/\`) || strings.Contains(sid, "..") {
		return "", false
	}
	return filepath.Join(os.TempDir(), "loto-beadwarn-"+sid), true
}

// claimBeadWarn reports whether this invocation owns the session's one bead-id
// nudge. O_EXCL makes the claim atomic across the concurrent loto processes a
// fleet session runs, so the warning cannot double-print. Any marker failure
// falls open to warning — a nudge printed twice beats a nudge lost.
func claimBeadWarn() bool {
	path, ok := beadWarnMarker()
	if !ok {
		return true
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return errors.Is(err, os.ErrPermission) || !errors.Is(err, os.ErrExist)
	}
	f.Close()
	return true
}

// runTag delivers to every current holder — and, when nobody holds the target,
// pins the note to that ground instead of refusing (loto-z3y1). The refusal it
// replaces read "not locked — acquire it yourself", which served the tag's old
// contract exactly and served the caller not at all: the one thing they could
// not do was leave word for whoever comes next.
func runTag(rt *runtime, canonical, text string, ttl time.Duration, ttlSet bool, stdout, stderr io.Writer) int {
	// Deliver to EVERY current holder, not an arbitrary one: a target held
	// shared by N agents has N blockers, and a note "on this file" must reach
	// each (loto-2nc5). InsertTag binds to the (target, owner, created_at)
	// triple, so one tag per holder.
	holders, err := rt.Store.LocksAt(rt.Ctx, domain.Target{Canonical: canonical})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if len(holders) == 0 {
		return runTerritoryTag(rt, canonical, text, ttl, stdout, stderr)
	}
	// A silently-ignored flag is worse than a refusal: --ttl means nothing on a
	// held target, whose note lives exactly as long as the lock does.
	if ttlSet {
		fmt.Fprintf(stderr, "✗ --ttl applies to territory tags; %s is locked\n", relPath(canonical))
		return 2
	}
	var ids []string
	var firstErr error
	for i := range holders {
		id, err := rt.Store.InsertTag(rt.Ctx, store.NewTag{
			TargetCanonical: domain.Canonical(canonical),
			LockOwnerUUID:   string(holders[i].OwnerUUID),
			LockCreatedAt:   holders[i].CreatedAt.UnixNano(),
			TaggerUUID:      rt.Agent.UUID,
			Text:            text,
		})
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ids = append(ids, id)
	}
	// All holders rejected the note (every per-holder cap reached, or the lock
	// dropped mid-loop): surface the first error, nothing delivered.
	if len(ids) == 0 {
		return tagInsertErr(firstErr, canonical, stderr)
	}
	if len(ids) == 1 {
		fmt.Fprintf(stdout, "✓ tag id=%s target=%s\n", ids[0], relPath(canonical))
		return 0
	}
	fmt.Fprintf(stdout, "✓ tag delivered=%d target=%s ids=%s\n", len(ids), relPath(canonical), strings.Join(ids, ","))
	return 0
}

func tagInsertErr(err error, canonical string, stderr io.Writer) int {
	switch {
	case errors.Is(err, store.ErrTagCapReached):
		fmt.Fprintf(stderr, "✗ tag cap reached on %s (5) — escalate channel\n", relPath(canonical))
	case errors.Is(err, store.ErrNoHostLock):
		// Race: lock dropped between LockAt and InsertTag.
		fmt.Fprintf(stderr, "✗ %s not locked — acquire it yourself\n", relPath(canonical))
	default:
		fmt.Fprintf(stderr, "✗ %v\n", err)
	}
	return 3
}

// runTerritoryTag pins a note to ground nobody holds. The confirmation is
// deliberately unlike the held-tag one — `✓ territory-tag id=tt-…` vs
// `✓ tag id=t-…` — so a mistyped path is visible here rather than three verbs
// later when the note fails to reach anyone (loto-z3y1 D9).
func runTerritoryTag(rt *runtime, prefix, text string, ttl time.Duration, stdout, stderr io.Writer) int {
	expires := time.Now().Add(ttl)
	id, err := rt.Store.InsertTerritoryTag(rt.Ctx, store.NewTerritoryTag{
		PathPrefix: prefix,
		TaggerUUID: rt.Agent.UUID,
		Text:       text,
		ExpiresAt:  expires.UnixNano(),
	})
	if err != nil {
		return territoryTagInsertErr(err, prefix, stderr)
	}
	fmt.Fprintf(stdout, "✓ territory-tag id=%s prefix=%s expires_at=%s\n",
		id, relPath(prefix), expires.UTC().Format(time.RFC3339))
	return 0
}

func territoryTagInsertErr(err error, prefix string, stderr io.Writer) int {
	switch {
	case errors.Is(err, store.ErrTagCapReached):
		fmt.Fprintf(stderr, "✗ territory-tag cap reached at %s — ack a live note first\n", relPath(prefix))
		fmt.Fprintf(stderr, "```bash\nloto status\n```\n")
	case errors.Is(err, store.ErrTagTextTooLong):
		fmt.Fprintln(stderr, "✗ territory-tag text exceeds the 4096-byte cap")
	case errors.Is(err, store.ErrTerritoryTagTTLTooLong):
		fmt.Fprintln(stderr, "✗ --ttl exceeds the 30d cap — a note that outlives a month is an inbox")
	default:
		fmt.Fprintf(stderr, "✗ %v\n", err)
	}
	return 3
}
