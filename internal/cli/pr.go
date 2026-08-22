package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"loto/internal/gate"
)

func init() { register("pr", cmdPR) } //nolint:gochecknoinits // command registry pattern

// prUsageHead is the point-of-use teaching surface (loto-5rwc), φ syncUsageHead.
const prUsageHead = `usage: loto pr [--dry-run] [--bead <id>] [--base <ref>] [--remote <name>]

Cross promoted work from refs/loto/integration to GitHub: group every promoted
commit by its Bead: trailer, build one branch per bead, push it, and open one
non-draft PR per bead carrying a Closes: trailer.

integration is machine-verified state; main is human-accepted state, and only
a person moves work between them — this verb builds the request, never the
merge. It never pushes to main and never merges anything.

Safe to run repeatedly: a bead already bridged is skipped, a bead that promoted
again since gets the new commits appended to its open branch, and a bead whose
paths moved on main is refused rather than replayed over.

`

// prNetTimeout bounds the two calls that leave the machine. Far longer than
// gitTimeout: `git push` and `gh` talk to GitHub, and a slow network is not
// the wedged-repo condition gitTimeout exists to catch.
const prNetTimeout = 120 * time.Second

// Row actions, in the order a reader meets them. The would-* pair is what a
// --dry-run row reports: what a real run would do next for that bead.
const (
	prActionOpened      = "opened"
	prActionUpdated     = "updated"
	prActionAlreadyOpen = "already-open"
	prActionWouldBuild  = "would-build"
	prActionWouldOpen   = "would-open"
	prActionBlocked     = "blocked"
)

// errPRNoURL marks a `gh pr create` that succeeded without printing the PR
// URL — infrastructure trouble, not a verdict.
var errPRNoURL = errors.New("gh pr create: no pull-request URL in output")

// errPRNotABeadBranch guards the push. loto pr writes bead branches and
// nothing else; main is moved by a person merging a PR, never by this verb.
var errPRNotABeadBranch = errors.New("loto pr: refusing to push a branch outside " + gate.BranchPrefix)

// prRef is one pull request as this verb needs it.
type prRef struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// prCreate is one non-draft PR to open. Draft is deliberately not a field:
// a non-draft PR appearing IS dk's review signal, and a draft reads as "still
// the agent's, not ready" (git-profile.md).
type prCreate struct {
	Base, Branch, Title, Body string
}

// prPublisher is the GitHub half of the bridge. An interface for one reason:
// internal/gate must stay gh-free and store-free, so every call that leaves
// the machine lives here at the CLI seam, behind a seam the tests replace.
type prPublisher interface {
	Push(ctx context.Context, repoTop, remote, branch string) error
	FindPR(ctx context.Context, repoTop, branch string) (ref prRef, found bool, err error)
	CreatePR(ctx context.Context, repoTop string, req prCreate) (prRef, error)
}

// newPRPublisher is the test seam, φ gate's promoteBeforePhase3Fn: a mutable
// package var swapped by a test, never by production code.
//
// ‡ It is a package var, which is why no test that swaps it may call
// t.Parallel.
var newPRPublisher = func() prPublisher { return ghPublisher{} } //nolint:gochecknoglobals // test seam for the network half; φ gate.promoteBeforePhase3Fn

type prOpts struct {
	dryRun bool
	bead   string
	base   string
	remote string
}

// prRow is one rendered line of the report, kept as fields rather than a
// formatted string so the counts and the fix block read the verdict rather
// than re-parsing prose out of the line they just wrote.
type prRow struct {
	bead    string
	line    string
	action  string
	reason  string
	target  string // the offending path, when the reason names one
	blocked bool
}

func cmdPR(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	var o prOpts
	fs := flag.NewFlagSet("pr", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.BoolVar(&o.dryRun, "dry-run", false, "report what would be built and opened; touch no ref and no remote")
	fs.StringVar(&o.bead, "bead", "", "limit the run to one bead id")
	fs.StringVar(&o.base, "base", gate.DefaultMainRef, "the branch every PR targets and every bead branch forks from")
	fs.StringVar(&o.remote, "remote", "origin", "git remote to push bead branches to")
	fs.Usage = func() {
		fmt.Fprint(stderr, prUsageHead)
		fs.PrintDefaults()
	}
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprint(stderr, prUsageHead)
		return 2
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return runPR(ctx, repoTop, o, stdout, stderr)
}

// runPR plans the crossing, acts on each bead, and reports. No store is
// opened: the bridge reads git refs and objects only, so there is nothing to
// look up and no identity GC owed.
func runPR(ctx context.Context, repoTop string, o prOpts, stdout, stderr io.Writer) int {
	plan, err := gate.PlanBridge(ctx, gate.BridgeParams{RepoTop: repoTop, MainRef: o.base, BeadID: o.bead})
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	if plan.IntegrationSHA == "" {
		// Explicit empty status: silence would read as a crash (design.md).
		fmt.Fprintln(stdout, "ℹ pr beads=0 opened=0 updated=0 current=0 blocked=0 unattributed=0 integration=absent")
		return 0
	}

	pub := newPRPublisher()
	counts := map[string]int{}
	rows := make([]prRow, 0, len(plan.Beads)+len(plan.Unattributed))
	for i := range plan.Beads {
		row, err := bridgeBead(ctx, repoTop, o, pub, &plan.Beads[i])
		if err != nil {
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 3
		}
		counts[row.action]++
		rows = append(rows, row)
	}
	rows = append(rows, unattributedRows(plan)...)

	emitPRReport(stdout, o, plan, counts, rows)
	for _, r := range rows {
		if r.blocked {
			return 1
		}
	}
	return 0
}

// bridgeBead carries one bead as far as it can go: build the branch, push it,
// and make sure exactly one open PR points at it.
//
// ‡ Convergent, not transactional. Each step is skipped when it has already
// happened, so a run interrupted anywhere is repaired by the next run rather
// than duplicated by it — which is the only reason `gh pr create` is reached
// through FindPR instead of being called and having its "already exists"
// error swallowed.
func bridgeBead(ctx context.Context, repoTop string, o prOpts, pub prPublisher, b *gate.BeadBridge) (prRow, error) {
	if blocked, ok := blockedRow(b); ok {
		return blocked, nil
	}
	built := b.Class == gate.BridgeBuildable
	if built && !o.dryRun {
		if err := gate.BuildBridge(ctx, gate.BridgeParams{RepoTop: repoTop, MainRef: o.base}, b); err != nil {
			return prRow{}, err
		}
	}
	if o.dryRun {
		action := prDryAction(built)
		return prRow{bead: b.BeadID, line: prRowLine(b, action, prRef{}), action: action}, nil
	}

	// ‡ The one hard stop. Everything upstream of here already names a
	// bead branch, so this can only fire on a bug — and the failure it would
	// otherwise be is `git push origin main`, which is exactly what this verb
	// exists NOT to do. Cheap to keep, catastrophic to omit.
	if !strings.HasPrefix(b.Branch, gate.BranchPrefix) {
		return prRow{}, fmt.Errorf("%w: %q", errPRNotABeadBranch, b.Branch)
	}
	if err := pub.Push(ctx, repoTop, o.remote, b.Branch); err != nil {
		return prRow{}, err
	}
	ref, found, err := pub.FindPR(ctx, repoTop, b.Branch)
	if err != nil {
		return prRow{}, err
	}
	action := prActionAlreadyOpen
	switch {
	case !found:
		if ref, err = pub.CreatePR(ctx, repoTop, prCreate{
			Base: o.base, Branch: b.Branch, Title: prTitle(b), Body: prBody(b, o.base),
		}); err != nil {
			return prRow{}, err
		}
		action = prActionOpened
	case built:
		action = prActionUpdated
	}
	return prRow{bead: b.BeadID, line: prRowLine(b, action, ref), action: action}, nil
}

// prDryAction names what a real run would do next. An up-to-date bead is
// would-open rather than "nothing": its branch is already built, but whether
// a PR points at it is a question only `gh` can answer, and --dry-run does
// not ask.
func prDryAction(built bool) string {
	if built {
		return prActionWouldBuild
	}
	return prActionWouldOpen
}

// blockedRow renders the two classes the bridge refuses. The field naming the
// blocker differs by class — `target=` for a path, as everywhere else in
// loto's output, and `detail=` for a ref-state word — so the reader can tell
// at a glance which kind of thing to go look at.
func blockedRow(b *gate.BeadBridge) (prRow, bool) {
	var blocker, target string
	switch b.Class {
	case gate.BridgeStaleBase:
		blocker, target = "target="+b.Detail, b.Detail
	case gate.BridgeStaleBranch:
		blocker = "detail=" + b.Detail
	case gate.BridgeBuildable, gate.BridgeBuilt, gate.BridgeUpToDate:
		return prRow{}, false
	default:
		return prRow{}, false
	}
	return prRow{
		bead:    b.BeadID,
		line:    fmt.Sprintf("%s bead=%s branch=%s reason=%s %s", glyphFail, b.BeadID, b.Branch, b.Class, blocker),
		action:  prActionBlocked,
		reason:  string(b.Class),
		target:  target,
		blocked: true,
	}, true
}

func prRowLine(b *gate.BeadBridge, action string, ref prRef) string {
	glyph := glyphPass
	if action == prActionAlreadyOpen {
		glyph = "ℹ"
	}
	line := fmt.Sprintf("%s bead=%s branch=%s commits=%d", glyph, b.BeadID, b.Branch, len(b.Pending))
	if ref.Number != 0 {
		line += fmt.Sprintf(" pr=%d", ref.Number)
	}
	return line + " action=" + action
}

// unattributedRows surfaces promoted commits the bridge cannot group. Advisory,
// not blocking: they hold up nobody's PR, but leaving them silent would let
// promoted work sit unbridged forever with no one told why.
func unattributedRows(plan gate.BridgePlan) []prRow {
	rows := make([]prRow, 0, len(plan.Unattributed))
	for i := range plan.Unattributed {
		c := &plan.Unattributed[i]
		rows = append(rows, prRow{
			line:   fmt.Sprintf("⚠ commit=%s reason=no-bead-trailer candidate=%s", shortSHA(c.SHA), orDash(c.CandidateID)),
			reason: "no-bead-trailer",
		})
	}
	return rows
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// emitPRReport renders triage counts then every row, sorted so the same repo
// state prints byte-identically (design.md).
func emitPRReport(w io.Writer, o prOpts, plan gate.BridgePlan, counts map[string]int, rows []prRow) {
	glyph := glyphPass
	if counts[prActionBlocked] > 0 {
		glyph = glyphFail
	}
	// ‡ A dry run reports zero opened and zero updated, because it opened and
	// updated nothing. What it would carry goes in its own `pending=` key —
	// folding it into `opened` would make the two modes' triage lines mean
	// different things under the same name.
	fmt.Fprintf(w, "%s pr beads=%d opened=%d updated=%d current=%d blocked=%d unattributed=%d",
		glyph, len(plan.Beads),
		counts[prActionOpened], counts[prActionUpdated],
		counts[prActionAlreadyOpen]+counts[prActionWouldOpen],
		counts[prActionBlocked], len(plan.Unattributed))
	if o.dryRun {
		fmt.Fprintf(w, " pending=%d dry-run=true", counts[prActionWouldBuild])
	}
	fmt.Fprintln(w)

	sorted := append([]prRow(nil), rows...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].line < sorted[j].line })
	for _, r := range sorted {
		fmt.Fprintln(w, r.line)
	}
	emitPRFixBlock(w, rows)
}

// emitPRFixBlock prints one runnable line per blocked bead. Sorted by bead so
// the block is byte-identical for the same input (design.md).
func emitPRFixBlock(w io.Writer, rows []prRow) {
	var fixes []string
	for _, r := range rows {
		switch r.reason {
		case string(gate.BridgeStaleBase):
			fixes = append(fixes, fmt.Sprintf(
				"git log -1 -- %s   # who moved it on main; %s must be re-promoted against that\n", r.target, r.bead))
		case string(gate.BridgeStaleBranch):
			fixes = append(fixes, fmt.Sprintf(
				"git branch -D %s%s && loto pr --bead %s   # drop the spent branch, rebuild from main\n",
				gate.BranchPrefix, r.bead, r.bead))
		}
	}
	if len(fixes) == 0 {
		return
	}
	sort.Strings(fixes)
	fmt.Fprintln(w, "```bash")
	for _, fix := range fixes {
		fmt.Fprint(w, fix)
	}
	fmt.Fprintln(w, "```")
}

// --- PR text --------------------------------------------------------------

// prTitleMax keeps a title scannable in a PR list; GitHub allows far more.
const prTitleMax = 72

// prTitle leads with the bead id — the thing dk routes on — then names the
// write-set, which is what a reviewer scans a PR list for.
func prTitle(b *gate.BeadBridge) string {
	title := b.BeadID + ": " + strings.Join(b.WriteSet, ", ")
	if len(title) <= prTitleMax {
		return title
	}
	// Truncate by runes, not bytes: a path may be UTF-8, and slicing one
	// mid-sequence would put a replacement character in the PR title.
	runes := []rune(title)
	for len(string(runes)) > prTitleMax-3 {
		runes = runes[:len(runes)-1]
	}
	return string(runes) + "..."
}

// prBody states what the branch is, what it carries, and the assumption the
// per-bead split rests on — the last of these because a reviewer who does not
// know about lease disjointness cannot tell whether an isolated branch is
// safe to merge on its own.
func prBody(b *gate.BeadBridge, base string) string {
	var s strings.Builder
	fmt.Fprintf(&s, "Bridged from `refs/loto/integration` by `loto pr` — one PR per bead.\n\n")
	fmt.Fprintf(&s, "Every commit here was machine-verified before it reached integration: the gate\n")
	fmt.Fprintf(&s, "replayed it onto the prospective integration state and ran the invariant verify\n")
	fmt.Fprintf(&s, "command against that state. This branch replays the same path transitions onto\n")
	fmt.Fprintf(&s, "`%s`, one commit per promoted candidate.\n\n", base)

	fmt.Fprintf(&s, "commits (oldest first):\n")
	for i := range b.Pending {
		c := &b.Pending[i]
		fmt.Fprintf(&s, "- `%s` candidate `%s`\n", shortSHA(c.SHA), orDash(c.CandidateID))
	}
	fmt.Fprintf(&s, "\nwrite-set:\n")
	for _, p := range b.WriteSet {
		fmt.Fprintf(&s, "- `%s`\n", p)
	}
	fmt.Fprintf(&s, "\nWhy this reviews as an isolated branch: lease disjointness means no other bead\n")
	fmt.Fprintf(&s, "held a write lock on these paths, so no other bead's promoted work belongs in\n")
	fmt.Fprintf(&s, "them. `loto pr` re-checks that per path — `%s` must still hold the content\n", base)
	fmt.Fprintf(&s, "integration recorded before this bead changed it — and refuses the bridge on a\n")
	fmt.Fprintf(&s, "mismatch rather than replaying over whatever landed in between.\n\n")
	fmt.Fprintf(&s, "Closes: %s\n", b.BeadID)
	return s.String()
}

// --- the GitHub half ------------------------------------------------------

// ghPublisher is the only place in loto that shells out to `gh`.
type ghPublisher struct{}

// Push sends the bead branch to the remote under the same name. No --force
// and no refspec rewrite: gate.BuildBridge only ever fast-forwards a branch,
// so a rejected non-fast-forward push means someone else moved it and the
// operator needs to hear about that, not have it overwritten.
func (ghPublisher) Push(ctx context.Context, repoTop, remote, branch string) error {
	_, err := prRun(ctx, repoTop, "git", "push", remote, branch)
	return err
}

func (ghPublisher) FindPR(ctx context.Context, repoTop, branch string) (prRef, bool, error) {
	out, err := prRun(ctx, repoTop, "gh", "pr", "list", "--head", branch, "--state", "open", "--limit", "1", "--json", "number,url")
	if err != nil {
		return prRef{}, false, err
	}
	var found []prRef
	if err := json.Unmarshal([]byte(out), &found); err != nil {
		return prRef{}, false, fmt.Errorf("gh pr list --head %s: %w", branch, err)
	}
	if len(found) == 0 {
		return prRef{}, false, nil
	}
	return found[0], true, nil
}

// CreatePR opens the PR ready for review. `gh pr create` prints the new PR's
// URL and nothing machine-readable, so the number is read back off the URL.
func (ghPublisher) CreatePR(ctx context.Context, repoTop string, req prCreate) (prRef, error) {
	out, err := prRun(ctx, repoTop, "gh", "pr", "create",
		"--base", req.Base, "--head", req.Branch, "--title", req.Title, "--body", req.Body)
	if err != nil {
		return prRef{}, err
	}
	for field := range strings.FieldsSeq(out) {
		if !strings.Contains(field, "/pull/") {
			continue
		}
		n, convErr := strconv.Atoi(field[strings.LastIndex(field, "/")+1:])
		if convErr == nil {
			return prRef{Number: n, URL: field}, nil
		}
	}
	return prRef{}, fmt.Errorf("%w: %q", errPRNoURL, strings.TrimSpace(out))
}

func prRun(ctx context.Context, repoTop, bin string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, prNetTimeout)
	defer cancel()
	// bin is a literal ("git"/"gh"); args are internal tokens and ref names
	// gate already validated, and nothing here reaches a shell.
	cmd := exec.CommandContext(c, bin, args...)
	cmd.Dir = repoTop
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
