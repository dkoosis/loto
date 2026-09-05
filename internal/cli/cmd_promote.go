package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"loto/internal/gate"
)

func init() { register("promote", cmdPromote) } //nolint:gochecknoinits // command registry pattern

// promoteUsageHead is the point-of-use teaching surface (loto-5rwc), φ prUsageHead.
const promoteUsageHead = `usage: loto promote [--max-batch N] [--max-rounds N] [--dry-run] -- <cmd> [args...]

Drain accepted candidates onto refs/loto/integration, in the pusher's own
process — no daemon. Each round claims a batch under a short flock, verifies
the prospective chain tip with NO lock held (admission keeps running through
it), then commits the whole batch in one CAS'd ref transaction.

This is the only verb that advances refs/loto/integration. loto submit fills
the queue in front of it; loto pr bridges what it lands onward to GitHub.

<cmd> is the gate-owned invariant command, run against the prospective chain
tip in a throwaway worktree. It is a parameter and not config until loto has
somewhere outside any candidate tree to keep one (internal/gate/promotion.go).

--dry-run reports the accepted candidates a run would consider, advances no
ref and runs no verify — so it needs no command.

exit: 0 nothing was rejected · 1 a candidate verified red on its own and was
retired · 3 verify could not reach a verdict, or git/store trouble.

examples:
  loto promote --dry-run
  loto promote -- make check
  loto promote --max-batch 1 -- go test ./...
`

// Row actions and the counters the triage line reports, named once so the
// counts and the rows cannot drift apart.
const (
	promoteActionWouldConsider = "would-consider"
	promoteVerifyPass          = "pass"
	promoteVerifyFail          = "fail"
	promoteVerifyInfra         = "infra"
	promoteIntegrationAbsent   = "absent"
)

// promoteDetailMax bounds a Detail on its row. A verify-red Detail carries the
// tail of the verify output, which is multi-line; one finding per line is the
// output contract (design.md), so it is flattened and cut here.
const promoteDetailMax = 200

type promoteOpts struct {
	maxBatch  int
	maxRounds int
	dryRun    bool
}

func cmdPromote(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var o promoteOpts
	fs := flag.NewFlagSet("promote", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.IntVar(&o.maxBatch, "max-batch", 0, "candidates per batch; 0 keeps the gate default")
	fs.IntVar(&o.maxRounds, "max-rounds", 0, "rounds per run; 0 keeps the gate default")
	fs.BoolVar(&o.dryRun, "dry-run", false, "report what a run would consider; advance no ref and run no verify")
	fs.Usage = func() {
		fmt.Fprint(stderr, promoteUsageHead)
		fs.PrintDefaults()
	}
	// No permuteWith: everything after the flags is the verify command, which
	// carries its own flags (e.g. -race) that must not be parsed as loto flags.
	// flag.Parse stops at the first non-flag token, leaving the rest in Args().
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if o.maxBatch < 0 || o.maxRounds < 0 {
		fmt.Fprintln(stderr, "✗ --max-batch and --max-rounds must be >= 0 (0 keeps the gate default)")
		return 2
	}
	verify := fs.Args()
	// Drop an optional "--" separator between the flags and the command, φ
	// cmd_verify.go.
	if len(verify) > 0 && verify[0] == "--" {
		verify = verify[1:]
	}
	if !o.dryRun && len(verify) == 0 {
		fmt.Fprintln(stderr, "✗ verify command required: loto promote -- <cmd> [args...]")
		return 2
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if o.dryRun {
		// No store and no identity: a dry run reads candidate refs and nothing
		// else, so there is no owner to attribute and no GC owed.
		return runPromoteDryRun(ctx, repoTop, stdout, stderr)
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()
	return runPromote(rt, repoTop, o, verify, stdout, stderr)
}

// runPromote calls gate.Promote and renders its result. The store is the
// ClaimReleaser: retiring a candidate's refs is what must also stop its
// durable claims from blocking overlapping acquisitions.
func runPromote(rt *runtime, repoTop string, o promoteOpts, verify []string, stdout, stderr io.Writer) int {
	// ‡ os.Getpid(), NOT stampPID's durable session pid. A promoting claim
	// names THIS pusher — the process holding the batch through the unlocked
	// verify — so a later pusher can ask whether it is still alive. Stamping
	// the long-lived Claude session pid (right for a lock, which must outlive
	// the one-shot CLI) would leave a crashed promote's claim reading as live
	// for as long as the session lasts, and nothing else would drain the queue.
	pid := os.Getpid()
	procStart, _ := identityProcStart(pid)
	res, err := gate.Promote(rt.Ctx, gate.PromoteParams{
		RepoTop:   repoTop,
		VerifyCmd: verify,
		MaxBatch:  o.maxBatch,
		MaxRounds: o.maxRounds,
		Claims:    rt.Store,
		Live:      rt.liveProbe(),
		Host:      rt.Host,
		PID:       pid,
		ProcStart: procStart,
	})
	// Promote returns what it managed to do alongside the error, so the report
	// is emitted either way — a run that promoted two candidates and then hit
	// git trouble must not print as if nothing happened.
	code := emitPromoteReport(stdout, res)
	if err != nil {
		fmt.Fprintf(stderr, "✗ promote: %v\n", err)
		return 3
	}
	return code
}

// runPromoteDryRun reports the accepted candidates a real run would consider.
// It does NOT re-run phase 1's freshness checks: whether a candidate still
// applies is judged against a snapshot taken under the promotion flock, and
// answering it here would be a second definition of eligibility that can
// disagree with the one that counts.
func runPromoteDryRun(ctx context.Context, repoTop string, stdout, stderr io.Writer) int {
	ids, err := gate.ListCandidateIDs(ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	counts := promoteCounts{candidates: len(ids)}
	fmt.Fprint(stdout, promoteTriageLine(counts, promoteIntegrationSHA(ctx, repoTop), true))
	for _, id := range ids {
		fmt.Fprintf(stdout, "ℹ candidate=%s action=%s\n", id, promoteActionWouldConsider)
	}
	return 0
}

// promoteIntegrationSHA reads refs/loto/integration without creating it.
// gate.ResolveIntegrationRef bootstraps the ref to HEAD on first use, which is
// a write no dry run may make.
func promoteIntegrationSHA(ctx context.Context, repoTop string) string {
	out, err := gitOutput(ctx, repoTop, "rev-parse", "--verify", "--quiet", gate.IntegrationRef)
	if err != nil {
		return promoteIntegrationAbsent
	}
	sha := strings.TrimSpace(out)
	if sha == "" {
		return promoteIntegrationAbsent
	}
	return shortSHA(sha)
}

// promoteCounts is one run's triage, kept as fields rather than a formatted
// string so the exit code and the first line read the same numbers.
type promoteCounts struct {
	candidates int
	promoted   int
	stale      int
	rejected   int
	requeued   int
	infra      int
	verifies   int
}

func tallyPromote(res gate.PromoteResult) promoteCounts {
	c := promoteCounts{candidates: len(res.Outcomes), verifies: len(res.Verifies)}
	for _, o := range res.Outcomes {
		switch o.Class {
		case gate.OutcomePromoted:
			c.promoted++
		case gate.OutcomeStalePreimage, gate.OutcomeStaleAncestry:
			c.stale++
		case gate.OutcomeVerifyRed:
			c.rejected++
		case gate.OutcomeVerifyInfra:
			c.infra++
		case gate.OutcomePromotionRace:
			c.requeued++
		}
	}
	return c
}

// promoteTriageLine is the first body line: every counter, then where
// integration now stands. Explicit even when everything is zero — silence
// reads as a crash (design.md).
func promoteTriageLine(c promoteCounts, integration string, dryRun bool) string {
	glyph := "✓"
	switch {
	case c.rejected > 0 || c.infra > 0:
		glyph = "✗"
	case c.candidates == 0:
		glyph = "ℹ"
	}
	line := fmt.Sprintf("%s promote candidates=%d promoted=%d stale=%d rejected=%d requeued=%d infra=%d verifies=%d integration=%s",
		glyph, c.candidates, c.promoted, c.stale, c.rejected, c.requeued, c.infra, c.verifies, integration)
	if dryRun {
		line += " dry-run=true"
	}
	return line + "\n"
}

// emitPromoteReport renders the triage line, one row per candidate, one row
// per verify, and a fix block for anything rejected. Returns the exit code.
//
// ‡ Outcomes arrive sorted by candidate id (gate.finish), and the verify rows
// stay in the order they ran — the order is what makes a red batch's n+1
// sweep legible. The only non-deterministic field is a verify's own duration,
// which is the measurement this verb exists to expose.
func emitPromoteReport(w io.Writer, res gate.PromoteResult) int {
	c := tallyPromote(res)
	integration := promoteIntegrationAbsent
	if res.Integration != "" {
		integration = shortSHA(res.Integration)
	}
	fmt.Fprint(w, promoteTriageLine(c, integration, false))

	for _, o := range res.Outcomes {
		line := fmt.Sprintf("%s candidate=%s bead=%s action=%s",
			promoteOutcomeGlyph(o.Class), o.CandidateID, orDash(o.BeadID), o.Class)
		if d := promoteDetail(o.Detail); d != "" {
			line += " detail=" + d
		}
		fmt.Fprintln(w, line)
	}
	for _, v := range res.Verifies {
		fmt.Fprintf(w, "ℹ verify batch=%s candidates=%d ms=%d result=%s\n",
			v.BatchID, v.Candidates, v.Duration.Milliseconds(), promoteVerifyResult(v))
	}
	emitPromoteFixBlock(w, res)

	switch {
	case c.infra > 0:
		return 3
	case c.rejected > 0:
		return 1
	}
	return 0
}

func promoteOutcomeGlyph(class gate.OutcomeClass) string {
	switch class {
	case gate.OutcomePromoted:
		return "✓"
	case gate.OutcomeVerifyRed, gate.OutcomeVerifyInfra:
		return "✗"
	case gate.OutcomeStalePreimage, gate.OutcomeStaleAncestry, gate.OutcomePromotionRace:
		return "⚠"
	}
	return "ℹ"
}

func promoteVerifyResult(v gate.VerifyRun) string {
	switch {
	case v.Infra:
		return promoteVerifyInfra
	case v.Passed:
		return promoteVerifyPass
	}
	return promoteVerifyFail
}

// promoteDetail flattens a Detail onto its row: newlines to spaces, bounded
// length. The full verify output is not lost — it is what the operator
// reproduces by running the invariant command themselves.
func promoteDetail(detail string) string {
	d := strings.Join(strings.Fields(detail), " ")
	if len(d) <= promoteDetailMax {
		return d
	}
	return d[:promoteDetailMax] + "…"
}

// emitPromoteFixBlock prints one runnable line per rejected candidate. A
// rejection deleted the candidate and proposal refs — there is nothing left to
// re-verify — so the remedy is always the same: fix the tree and resubmit.
func emitPromoteFixBlock(w io.Writer, res gate.PromoteResult) {
	var fixes []string
	for _, o := range res.Outcomes {
		if o.Class != gate.OutcomeVerifyRed {
			continue
		}
		fixes = append(fixes, fmt.Sprintf(
			"loto submit <file>... --bead %s   # %s verified red alone and was retired\n",
			orDash(o.BeadID), o.CandidateID))
	}
	if len(fixes) == 0 {
		return
	}
	fmt.Fprintln(w, "```bash")
	for _, fix := range fixes {
		fmt.Fprint(w, fix)
	}
	fmt.Fprintln(w, "```")
}
