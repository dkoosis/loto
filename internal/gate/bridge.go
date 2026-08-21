//go:build unix

package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// The bridge is the crossing from the third authority level to the fourth
// (git-gate.md): refs/loto/integration holds machine-verified state, GitHub
// main holds human-accepted state, and only a person moves work between them.
// This file builds the artifact that ASKS for that crossing — one branch per
// bead — and stops there. Pushing the branch and opening the PR is the CLI's
// job; this package stays store-free and gh-free so the whole grouping,
// replay and staleness story is testable with zero network.
//
// ‡ One branch PER BEAD is safe only because of lease disjointness: two beads
// never hold write locks on the same path, so their promoted transitions
// touch disjoint path sets and each branch can be replayed onto main
// independently, in any order, without a merge. planStaleBase is the check
// that catches it if that assumption is ever violated in practice — a bead
// whose paths moved under it on main is refused, never silently overwritten.

const (
	// DefaultMainRef is the human-accepted authority every per-bead branch is
	// built on and every PR targets.
	DefaultMainRef = "main"

	// BranchPrefix is the per-bead branch namespace, exported because the CLI
	// that pushes these branches must be able to refuse anything outside it —
	// one home for the fact, so the guard cannot drift from the builder.
	//
	// Deliberately NOT nested under loto/, which internal/lane already owns
	// for lane refs: git's loose-ref storage cannot hold both a ref file at
	// refs/heads/loto/pr and a directory of refs beneath the same path, so a
	// lane happening to be named "pr" would make the two namespaces mutually
	// exclusive.
	BranchPrefix       = "loto-pr/"
	bridgeBranchPrefix = "refs/heads/" + BranchPrefix

	// bridgedRefPrefix records, per bead, the LAST integration commit already
	// replayed onto that bead's branch. It is written in the same ref
	// transaction as the branch itself, so the marker can never claim more
	// (or less) than the branch actually carries — that atomicity is the
	// whole of the idempotence story, and it is why a crash between the two
	// writes cannot leave a run that duplicates commits on re-entry.
	bridgedRefPrefix = "refs/loto/bridged/"

	// bridgeSourceTrailer names the integration commit a branch commit was
	// replayed from. Provenance for a reviewer, and the one field that makes
	// a rebuilt branch auditable against integration by eye.
	bridgeSourceTrailer = "Source"

	// trailerBead is the grouping key: promotion stamps it on every chain
	// commit from the envelope's BeadID.
	trailerBead      = "Bead"
	trailerCandidate = "Candidate"
)

// bridgeLogFormat pulls, per promoted commit, everything a faithful replay
// needs: the SHA, both identities with their dates (copied verbatim so a
// rebuild is byte-identical — see BuildBridge), and the full message.
// \x1f separates fields, \x1e separates records; neither can occur in a git
// identity or in a commit message git will round-trip.
const bridgeLogFormat = "%H%x1f%an%x1f%ae%x1f%aI%x1f%cn%x1f%ce%x1f%cI%x1f%B%x1e"

const bridgeLogFieldCount = 8

// BridgeClass is one bead's verdict. Exactly one per bead per run.
type BridgeClass string

const (
	// BridgeBuildable: PlanBridge found pending commits that replay cleanly.
	// BuildBridge turns this into BridgeBuilt.
	BridgeBuildable BridgeClass = "buildable"
	// BridgeBuilt: the branch ref and the bridged marker now carry every
	// pending commit.
	BridgeBuilt BridgeClass = "built"
	// BridgeUpToDate: every promoted commit for this bead is already on its
	// branch. Nothing to build — but the CLI may still owe it a push or a PR.
	BridgeUpToDate BridgeClass = "up-to-date"
	// BridgeStaleBase: a path this bead promoted no longer holds, on the
	// branch's parent, the content integration recorded before the bead
	// touched it. Replaying would silently revert whoever changed it. Refused.
	BridgeStaleBase BridgeClass = "stale-base"
	// BridgeStaleBranch: the branch ref and the bridged marker disagree about
	// what has been bridged. Refused rather than guessed at — every shape of
	// this disagreement is a human deleting or hand-building a ref, and every
	// repair is one command.
	BridgeStaleBranch BridgeClass = "stale-branch"
)

// Stale-branch details. Each names which of the two refs is the surprise.
const (
	detailBranchMissing = "branch-missing"
	detailUnmarked      = "unmarked-branch"
	detailMergedBranch  = "merged-branch"
)

// BridgeParams is PlanBridge's and BuildBridge's shared input.
type BridgeParams struct {
	// RepoTop is the working-tree root; every git command runs there. The
	// tree itself is never read or written — this is all ref and object
	// plumbing, so N agents can share the checkout while it runs.
	RepoTop string
	// MainRef is the human-accepted authority. Empty → DefaultMainRef.
	MainRef string
	// BeadID, when set, limits the plan to that one bead.
	BeadID string
}

// BridgeCommit is one promoted commit as the bridge needs it: identity and
// dates to replay it faithfully, and the transitions to replay.
type BridgeCommit struct {
	SHA         string
	CandidateID string
	Message     string
	// Author/Committer name, email and ISO-8601 date, copied from the source
	// commit. Replaying with the same values is what makes a rebuild produce
	// the same SHA (BuildBridge).
	AuthorName, AuthorEmail, AuthorDate          string
	CommitterName, CommitterEmail, CommitterDate string
	// Transitions is the commit's diff against its first parent, in the same
	// shape promotion applies: Result nil means the path is deleted. Expected
	// is the parent's blob — what the branch's base must still hold.
	Transitions []Transition
}

// BeadBridge is one bead's crossing.
type BeadBridge struct {
	BeadID string
	// Branch is the short name (`loto-pr/<bead>`) — what `git push` and
	// `gh pr create --head` want. Ref is the full refs/heads/... spelling.
	Branch, Ref string
	// Parent is the commit the pending commits replay onto: the existing
	// branch tip when the branch already carries earlier work for this bead,
	// else main's tip. Head is the branch tip after BuildBridge.
	Parent, Head string
	// Pending is the promoted commits not yet on the branch, in integration
	// order. WriteSet is their sorted union of paths.
	Pending  []BridgeCommit
	WriteSet []string
	Class    BridgeClass
	Detail   string

	// hadBranch/markerSHA are what BuildBridge's ref transaction CASes
	// against. Unexported: a caller passes the value back untouched, and
	// nothing outside this package has any business synthesizing them.
	hadBranch bool
	markerSHA string
	markerRef string
}

// BridgePlan is one run's read-only verdict.
type BridgePlan struct {
	MainSHA string
	// IntegrationSHA is empty when refs/loto/integration does not exist —
	// nothing has ever been promoted, which is a neutral state, not an error.
	IntegrationSHA string
	// Beads is sorted by BeadID, so the same repo state renders byte-identically.
	Beads []BeadBridge
	// Unattributed holds promoted commits carrying no usable Bead: trailer,
	// in integration order. They are reported, never bridged — see
	// beadIDUsableAsRef.
	Unattributed []BridgeCommit
}

var (
	// ErrBridgeInput is a caller bug — a required parameter left blank.
	ErrBridgeInput = errors.New("gate: invalid bridge input")
	// errBridgeNotBuildable guards BuildBridge against a bead PlanBridge
	// already refused; building one would write a branch the plan said not to.
	errBridgeNotBuildable = errors.New("gate: bead is not buildable")
	// errBridgeDiffStatus marks a diff-tree status the replay has no rule
	// for. Rename/copy detection is off, so only M/A/D/T can appear; anything
	// else means git's output shape changed under us.
	errBridgeDiffStatus = errors.New("gate: unhandled diff-tree status")
	// errBridgeMalformedLog marks a git-log record that did not split into
	// the expected field count.
	errBridgeMalformedLog = errors.New("gate: malformed bridge log record")
)

// PlanBridge groups every promoted-but-unmerged commit by its Bead: trailer
// and decides, per bead, whether its branch can be built. Read-only: it
// writes no ref, no object, and never touches the working tree.
//
// ‡ Ordering. Within a bead, commits keep integration order — that is the
// order promotion verified them in, and the only order whose intermediate
// states were ever checked. ACROSS beads there is deliberately no order at
// all: each branch is replayed onto main independently, because lease
// disjointness means no two beads' write-sets intersect. Interleaving in
// integration is therefore an artifact of when candidates arrived, not a
// dependency, and reproducing it across branches would invent a constraint
// the gate never asserted.
func PlanBridge(ctx context.Context, p BridgeParams) (BridgePlan, error) {
	p, err := p.normalize()
	if err != nil {
		return BridgePlan{}, err
	}
	g := gitRunner{repoTop: p.RepoTop}

	mainSHA, err := g.run(ctx, "rev-parse", "--verify", p.MainRef+"^{commit}")
	if err != nil {
		return BridgePlan{}, fmt.Errorf("gate: resolve %s: %w", p.MainRef, err)
	}
	plan := BridgePlan{MainSHA: mainSHA}

	// Read-only, and deliberately NOT ResolveIntegrationRef: that bootstraps
	// the ref to HEAD as a side effect, and a verb that only reports on
	// integrated state must not mint the authority it reports on (φ
	// cmd_sync.go's syncIntegrationSHA, same reasoning).
	integSHA, integrated, err := resolveRef(ctx, p.RepoTop, IntegrationRef)
	if err != nil {
		return BridgePlan{}, err
	}
	if !integrated {
		return plan, nil
	}
	plan.IntegrationSHA = integSHA

	commits, err := readPromotedCommits(ctx, p.RepoTop, p.MainRef)
	if err != nil {
		return BridgePlan{}, err
	}

	byBead, order := groupByBead(commits, &plan)
	for _, bead := range order {
		if p.BeadID != "" && bead != p.BeadID {
			continue
		}
		b, err := planBead(ctx, p, mainSHA, bead, byBead[bead])
		if err != nil {
			return BridgePlan{}, err
		}
		plan.Beads = append(plan.Beads, b)
	}
	sort.Slice(plan.Beads, func(i, j int) bool { return plan.Beads[i].BeadID < plan.Beads[j].BeadID })
	return plan, nil
}

func (p BridgeParams) normalize() (BridgeParams, error) {
	if p.RepoTop == "" {
		return p, fmt.Errorf("%w: RepoTop", ErrBridgeInput)
	}
	if p.MainRef == "" {
		p.MainRef = DefaultMainRef
	}
	return p, nil
}

// groupByBead partitions promoted commits by their Bead: trailer, preserving
// integration order within each bead and first-appearance order across beads.
// A commit whose bead id is missing or unusable as a ref component lands in
// plan.Unattributed instead: the bridge cannot name a `Closes:` for it and
// cannot group it, and folding it into some other bead's PR would attribute a
// change to work that never asked for it.
func groupByBead(commits []BridgeCommit, plan *BridgePlan) (byBead map[string][]BridgeCommit, order []string) {
	byBead = map[string][]BridgeCommit{}
	for i := range commits {
		bead := commits[i].beadID()
		if !beadIDUsableAsRef(bead) {
			plan.Unattributed = append(plan.Unattributed, commits[i])
			continue
		}
		if _, seen := byBead[bead]; !seen {
			order = append(order, bead)
		}
		byBead[bead] = append(byBead[bead], commits[i])
	}
	return byBead, order
}

func (c BridgeCommit) beadID() string { return parseTrailers(c.Message)[trailerBead] }

// planBead decides one bead's class and, when buildable, loads the pending
// commits' transitions and checks them against the branch's parent.
func planBead(ctx context.Context, p BridgeParams, mainSHA, bead string, commits []BridgeCommit) (BeadBridge, error) {
	b := BeadBridge{
		BeadID:    bead,
		Ref:       bridgeBranchPrefix + bead,
		Branch:    strings.TrimPrefix(bridgeBranchPrefix+bead, "refs/heads/"),
		markerRef: bridgedRefPrefix + bead,
	}
	branchTip, hadBranch, err := resolveRef(ctx, p.RepoTop, b.Ref)
	if err != nil {
		return b, err
	}
	markerSHA, hadMarker, err := resolveRef(ctx, p.RepoTop, b.markerRef)
	if err != nil {
		return b, err
	}
	b.hadBranch, b.markerSHA = hadBranch, markerSHA

	pending, class, detail := selectPending(commits, markerSHA, hadMarker, hadBranch)
	if class != "" {
		b.Class, b.Detail, b.Parent, b.Head = class, detail, branchTip, branchTip
		return b, nil
	}

	b.Parent = mainSHA
	if hadBranch {
		b.Parent = branchTip
	}
	b.Head = b.Parent
	if len(pending) == 0 {
		b.Class = BridgeUpToDate
		return b, nil
	}

	b.Pending = pending
	if err := loadTransitions(ctx, p.RepoTop, b.Pending); err != nil {
		return b, err
	}
	b.WriteSet = aggregateWriteSet(b.Pending)

	stale, err := planStaleBase(ctx, p.RepoTop, b.Parent, b.Pending)
	if err != nil {
		return b, err
	}
	if stale != "" {
		b.Class, b.Detail = BridgeStaleBase, stale
		return b, nil
	}
	b.Class = BridgeBuildable
	return b, nil
}

// selectPending applies the bridged marker to one bead's commit list, or
// refuses when the marker and the branch tell different stories.
//
// ‡ The list it filters is `main..refs/loto/integration`, so a commit already
// reachable from main is not in it at all. That is what makes "the marker
// names a commit this list does not contain" a POSITIVE signal rather than an
// ambiguity: the previously-bridged work landed on main. The only other way
// to produce it is rewriting integration's history, which the gate does not
// do and the plan does not contemplate.
func selectPending(commits []BridgeCommit, markerSHA string, hadMarker, hadBranch bool) (pending []BridgeCommit, class BridgeClass, detail string) {
	markerAt := -1
	if hadMarker {
		for i := range commits {
			if commits[i].SHA == markerSHA {
				markerAt = i
				break
			}
		}
	}
	switch {
	case markerAt >= 0 && !hadBranch:
		// The marker's commits are still unmerged, so an open PR is very
		// likely pointing at a branch someone deleted underneath it.
		return nil, BridgeStaleBranch, detailBranchMissing
	case markerAt >= 0:
		return commits[markerAt+1:], "", ""
	case hadMarker && hadBranch:
		// The bridged work landed on main, but the branch that carried it is
		// still here. Appending to it would put already-merged commits in the
		// next PR; resetting it would rewrite a branch this package never
		// rewrites. The operator deletes it.
		return nil, BridgeStaleBranch, detailMergedBranch
	case hadBranch:
		// A branch with no marker was not built here — every branch this
		// package writes gets its marker in the same ref transaction.
		return nil, BridgeStaleBranch, detailUnmarked
	default:
		return commits, "", ""
	}
}

// planStaleBase re-checks, at the integration→main boundary, the same
// invariant promotion checks at the candidate→integration one: every path
// this bead is about to write must still hold, on the branch's parent, the
// content integration recorded immediately before the bead touched it.
//
// ‡ Lease disjointness protects a bead from its PEERS, not from main. main
// moves by PR merge and by hand, and neither takes a loto lease. Without this
// check a replay would set a path back to the blob integration knew about,
// silently reverting whatever landed on main in between. Returning the
// offending path is the whole report the operator needs.
func planStaleBase(ctx context.Context, repoTop, parent string, pending []BridgeCommit) (detail string, err error) {
	g := gitRunner{repoTop: repoTop}
	// The expected preimage is the one recorded by the FIRST pending commit
	// to touch the path: later commits in the same bead expect their own
	// predecessor's result, which the replay produces on the branch itself.
	expected := map[string]*BlobRef{}
	var paths []string
	for i := range pending {
		for _, tr := range pending[i].Transitions {
			if _, seen := expected[tr.Path]; seen {
				continue
			}
			expected[tr.Path] = tr.Expected
			paths = append(paths, tr.Path)
		}
	}
	sort.Strings(paths)

	for _, path := range paths {
		have, err := g.blobAt(ctx, parent, path)
		if err != nil {
			return "", err
		}
		if !sameBlob(have, expected[path]) {
			return path, nil
		}
	}
	return "", nil
}

func sameBlob(a, b *BlobRef) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.SHA == b.SHA && a.Mode == b.Mode
}

// BuildBridge replays a buildable bead's pending commits onto its parent and
// publishes the branch ref and the bridged marker in ONE ref transaction.
//
// ‡ Plumbing only — a scratch index under os.MkdirTemp, write-tree,
// commit-tree, update-ref. No checkout, no HEAD move, no shared-index write,
// so it is safe with N agents editing the same working tree (φ internal/lane,
// which exists for exactly this reason).
//
// ‡ Every replayed commit reuses the source commit's author, committer and
// both dates. That is what makes the output deterministic: the same parent
// and the same pending commits produce the same SHA on every run, so a
// re-entry after a crash rebuilds identical objects rather than a divergent
// history.
func BuildBridge(ctx context.Context, p BridgeParams, b *BeadBridge) error {
	p, err := p.normalize()
	if err != nil {
		return err
	}
	if b.Class != BridgeBuildable || len(b.Pending) == 0 {
		return fmt.Errorf("%w: %s is %q", errBridgeNotBuildable, b.BeadID, b.Class)
	}

	tmp, err := os.MkdirTemp("", "loto-bridge-")
	if err != nil {
		return fmt.Errorf("gate: bridge tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")

	head := b.Parent
	for i := range b.Pending {
		c := &b.Pending[i]
		tree, err := applyTransitions(ctx, p.RepoTop, idx, head, c.Transitions)
		if err != nil {
			return err
		}
		head, err = bridgeCommitTree(ctx, p.RepoTop, tree, head, *c)
		if err != nil {
			return err
		}
	}

	last := b.Pending[len(b.Pending)-1].SHA
	if err := UpdateRefsTx(ctx, p.RepoTop, []RefUpdate{
		refWrite(b.Ref, head, b.Parent, b.hadBranch),
		refWrite(b.markerRef, last, b.markerSHA, b.markerSHA != ""),
	}); err != nil {
		return err
	}
	b.Head, b.Class = head, BridgeBuilt
	return nil
}

// stripBatchTrailers removes promotion's BATCH-level bookkeeping from a
// replayed message, keeping the per-candidate attribution.
//
// ‡ Not cosmetic. Those trailers ride on a chain's TIP commit and describe
// the promoting process — its host, its pid, and the other candidates it
// happened to batch alongside this one. `Batch-Candidates` therefore names
// candidates belonging to OTHER BEADS, and carrying it onto a per-bead branch
// would put an id in this PR that is provably not in this PR. The line
// splitting matches parseTrailers exactly, so what this drops is exactly what
// the trailer reader would have seen.
func stripBatchTrailers(msg string) string {
	var kept []string
	for line := range strings.SplitSeq(msg, "\n") {
		if key, _, ok := strings.Cut(strings.TrimSpace(line), ": "); ok && isBatchTrailer(key) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func isBatchTrailer(key string) bool {
	switch key {
	case trailerBatch, trailerCandidates, trailerHost, trailerPID, trailerProcStart:
		return true
	default:
		return false
	}
}

// refWrite picks create vs. compare-and-swap update. The CAS is what makes a
// concurrent second bridge run lose loudly instead of overwriting the branch
// the first one just published.
func refWrite(ref, newSHA, oldSHA string, exists bool) RefUpdate {
	if !exists {
		return RefUpdate{Verb: VerbCreate, Ref: ref, NewSHA: newSHA}
	}
	return RefUpdate{Verb: VerbUpdate, Ref: ref, NewSHA: newSHA, OldSHA: oldSHA}
}

// bridgeCommitTree writes one replayed commit: the source's per-candidate
// attribution, the batch bookkeeping dropped, and the Source: trailer that
// names where it came from.
func bridgeCommitTree(ctx context.Context, repoTop, tree, parent string, c BridgeCommit) (string, error) {
	msg := stripBatchTrailers(c.Message)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	msg += fmt.Sprintf("%s: %s\n", bridgeSourceTrailer, c.SHA)

	cmd := exec.CommandContext(ctx, "git", "commit-tree", tree, "-p", parent)
	cmd.Dir = repoTop
	cmd.Stdin = strings.NewReader(msg)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME="+c.AuthorName, "GIT_AUTHOR_EMAIL="+c.AuthorEmail, "GIT_AUTHOR_DATE="+c.AuthorDate,
		"GIT_COMMITTER_NAME="+c.CommitterName, "GIT_COMMITTER_EMAIL="+c.CommitterEmail, "GIT_COMMITTER_DATE="+c.CommitterDate,
	)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gate: bridge commit-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// readPromotedCommits lists `<mainRef>..refs/loto/integration` oldest-first —
// every promoted commit not yet reachable from main, in the order promotion
// verified them.
func readPromotedCommits(ctx context.Context, repoTop, mainRef string) ([]BridgeCommit, error) {
	g := gitRunner{repoTop: repoTop}
	out, err := g.run(ctx, "log", "--reverse", "--format="+bridgeLogFormat, mainRef+".."+IntegrationRef)
	if err != nil {
		return nil, fmt.Errorf("gate: log %s..%s: %w", mainRef, IntegrationRef, err)
	}
	var commits []BridgeCommit
	for rec := range strings.SplitSeq(out, "\x1e") {
		rec = strings.Trim(rec, "\n")
		if rec == "" {
			continue
		}
		f := strings.Split(rec, "\x1f")
		if len(f) != bridgeLogFieldCount {
			return nil, fmt.Errorf("%w: %d fields", errBridgeMalformedLog, len(f))
		}
		c := BridgeCommit{
			SHA: f[0], AuthorName: f[1], AuthorEmail: f[2], AuthorDate: f[3],
			CommitterName: f[4], CommitterEmail: f[5], CommitterDate: f[6],
			// %B keeps the message's own trailing newline; git adds one more
			// before the record separator, so trim only what git added.
			Message: strings.TrimSuffix(f[7], "\n"),
		}
		c.CandidateID = parseTrailers(c.Message)[trailerCandidate]
		commits = append(commits, c)
	}
	return commits, nil
}

// loadTransitions fills each commit's diff against its first parent.
func loadTransitions(ctx context.Context, repoTop string, commits []BridgeCommit) error {
	for i := range commits {
		trs, err := commitTransitions(ctx, repoTop, commits[i].SHA)
		if err != nil {
			return err
		}
		commits[i].Transitions = trs
	}
	return nil
}

// commitTransitions reconstructs one promoted commit's path transitions from
// git alone.
//
// ‡ The envelope that originally declared them is gone: promotion retires
// refs/loto/candidates/<id> the moment the chain lands. Re-deriving from the
// commit is not a fallback — the commit IS the record now, and deriving from
// it is what makes the bridge work on integration history it did not itself
// promote.
func commitTransitions(ctx context.Context, repoTop, sha string) ([]Transition, error) {
	g := gitRunner{repoTop: repoTop}
	raw, err := g.run(ctx, "diff-tree", "-r", "-z", "--no-commit-id", "--name-status", sha)
	if err != nil {
		return nil, fmt.Errorf("gate: diff-tree %s: %w", sha, err)
	}
	fields := strings.Split(strings.TrimRight(raw, "\x00"), "\x00")

	var trs []Transition
	for i := 0; i+1 < len(fields); i += 2 {
		status, path := fields[i], fields[i+1]
		if status == "" || path == "" {
			continue
		}
		// Rename and copy detection are off by default for diff-tree, so a
		// status is always one letter and always names exactly one path.
		switch status[0] {
		case 'M', 'A', 'T':
		case 'D':
			trs = append(trs, Transition{Path: path})
			continue
		default:
			return nil, fmt.Errorf("%w: %q at %s:%s", errBridgeDiffStatus, status, sha, path)
		}
		result, err := g.blobAt(ctx, sha, path)
		if err != nil {
			return nil, err
		}
		trs = append(trs, Transition{Path: path, Result: result})
	}

	// Expected is read from the commit's first parent — the integration state
	// immediately before this commit, which is exactly what the branch's base
	// must still agree with.
	for i := range trs {
		expected, err := g.blobAt(ctx, sha+"^", trs[i].Path)
		if err != nil {
			return nil, err
		}
		trs[i].Expected = expected
	}
	sort.Slice(trs, func(i, j int) bool { return trs[i].Path < trs[j].Path })
	return trs, nil
}

// aggregateWriteSet is the sorted union of every pending commit's paths — the
// bead's whole footprint, which is what a PR title and body should name.
func aggregateWriteSet(pending []BridgeCommit) []string {
	seen := map[string]bool{}
	var out []string
	for i := range pending {
		for _, tr := range pending[i].Transitions {
			if !seen[tr.Path] {
				seen[tr.Path] = true
				out = append(out, tr.Path)
			}
		}
	}
	sort.Strings(out)
	return out
}

// resolveRef returns ref's object SHA, reporting absence as ok=false rather
// than an error — every ref this package reads is legitimately missing on a
// bead's first crossing.
//
// ‡ for-each-ref, NOT `rev-parse --verify --quiet`. An absent ref is an
// ordinary empty result here, so a genuine git failure still surfaces as an
// error instead of being indistinguishable from absence (φ refs.go's
// abort-on-error contract: reading "no ref" out of a FAILED read is exactly
// what would make the bridge rebuild a branch that already exists).
func resolveRef(ctx context.Context, repoTop, ref string) (sha string, ok bool, err error) {
	out, err := gitRunner{repoTop: repoTop}.run(ctx, "for-each-ref", "--format=%(objectname)", ref)
	if err != nil {
		return "", false, fmt.Errorf("gate: for-each-ref %s: %w", ref, err)
	}
	if out == "" {
		return "", false, nil
	}
	return out, true, nil
}

// beadIDUsableAsRef reports whether a bead id can be a single ref component
// under both bridgeBranchPrefix and bridgedRefPrefix.
//
// ‡ Deliberately narrow — letters, digits, dot, dash, underscore. Two bead
// ids must never collapse onto one branch, so this REJECTS what it cannot
// carry verbatim rather than sanitizing it into a collision. Real bead ids
// (`loto-ovno.8`) pass; an empty trailer, a path-shaped id, or anything
// git-refname(7) would refuse does not.
func beadIDUsableAsRef(id string) bool {
	if id == "" || strings.HasPrefix(id, ".") || strings.HasPrefix(id, "-") ||
		strings.HasSuffix(id, ".") || strings.HasSuffix(id, ".lock") || strings.Contains(id, "..") {
		return false
	}
	return !strings.ContainsFunc(id, notRefRune)
}

// notRefRune reports a character that cannot appear in a bead id carried
// verbatim as a ref component.
func notRefRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return false
	case r == '.', r == '-', r == '_':
		return false
	default:
		return true
	}
}
