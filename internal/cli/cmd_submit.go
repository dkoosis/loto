package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
	"loto/internal/identity"
	"loto/internal/lane"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("submit", cmdSubmit) } //nolint:gochecknoinits // command registry pattern

// submitUsageHead is the point-of-use teaching surface (loto-5rwc).
const submitUsageHead = `usage: loto submit <file> [<file>...] --bead <id> [-m "<msg>"]

Package your held locks' current edits into a candidate for the git-gate
pipeline: lease check -> lane.Commit -> envelope capture -> admission.

On accept: writes refs/loto/candidates/<id> + refs/loto/proposals/<id>, and
converts each lease into a durable candidate claim. On reject: no candidate
or claim state is written — fix the reported reason and resubmit. Every
attempt, accepted or not, leaves one audit event (see: loto gate stats) and
may record a violation-scan finding (see: loto violations); neither is
candidate/claim state and neither blocks a resubmit. This is the first point
where the whole front half of the gate is observable in one command
(loto-ovno.5).

examples:
  loto submit internal/store/store.go --bead loto-ovno.5
  loto submit a.go b.go --bead loto-abc -m "two-file candidate"
`

func cmdSubmit(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("submit", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, submitUsageHead)
		fs.PrintDefaults()
	}
	bead := fs.String("bead", "", "bead id this candidate serves (required)")
	msg := fs.String("m", "", "commit message; defaults to the bead id")
	fs.StringVar(msg, "message", "", "commit message; defaults to the bead id")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if *bead == "" {
		fmt.Fprintln(stderr, "✗ --bead required: loto submit <file>... --bead <id>")
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprint(stderr, submitUsageHead)
		return 2
	}
	if *msg == "" {
		*msg = *bead
	}

	repoTop, err := repoTopForCwd(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	// Reuses lane's own write-set resolver (cmd_lane.go): no Lstat — a
	// candidate legitimately includes a deletion of a removed file, and
	// lane.Commit's own validateWriteSet is what checks on-disk shape.
	targets, invalid := resolveLaneWriteSet(fs.Args(), repoTop)
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}

	rt, err := openRuntimeGC(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	return runSubmit(rt, repoTop, *bead, *msg, targets, stdout, stderr)
}

// runSubmit is the vertical slice git-gate.md names: lease check -> lane.Commit
// -> envelope capture -> admission -> (accept: write refs + durable claims |
// reject: render why, write nothing).
func runSubmit(rt *runtime, repoTop, bead, msg string, targets []domain.Target, stdout, stderr io.Writer) int {
	owner := domain.AgentUUID(rt.Agent.UUID)
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe(), CaseFold: rt.CaseFold}

	// 1. LEASE CHECK — "fail at the cheap end": no git plumbing, no envelope,
	// no admission call until every target is confirmed held, live, exclusive.
	writeSet, epochs, code := submitLeaseCheck(rt, ec, owner, targets, stdout, stderr)
	if code != 0 {
		return code
	}

	if submitAfterLeaseCheck != nil {
		submitAfterLeaseCheck(rt)
	}

	// 2+3. Resolve the integration ref (bootstraps to HEAD on first use), then
	// lane.Commit — private index, exact write-set, no HEAD move.
	integrationRef, candidateID, proposal, code := submitBuildProposal(rt, repoTop, bead, msg, writeSet, stderr)
	if code != 0 {
		return code
	}

	// ‡ Every path below this point that does NOT accept must drop the lane
	// branch lane.Commit just wrote (Codex #259 P2). `loto submit` documents
	// "on reject: no candidate or claim state is written" — without this,
	// every rejected submit leaves a permanent refs/heads/loto/<id> behind,
	// and a retry loop leaves one per attempt.
	accepted := false
	defer func() {
		if !accepted {
			discardLaneRef(rt.Ctx, repoTop, candidateID, stderr)
		}
	}()

	// 4. Envelope capture. ONLY ErrAncestryNotTree / ErrPathNotBlob are
	// candidate verdicts ("this candidate is already invalid") — rendered as a
	// rejection so the operator sees admission's own triage-first shape. Any
	// other error is plumbing (ctx cancelled, git ls-tree failed on
	// permissions or a corrupt object store) and must exit 3, not masquerade
	// as a verdict the operator can fix by editing their candidate.
	env, err := gate.Capture(rt.Ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integrationRef, ProposalSHA: proposal,
		Base: integrationRef, WriteSet: writeSet, CandidateID: candidateID,
		Agent: owner, Session: rt.SessionUUID, BeadID: bead, LeaseEpoch: epochs,
	})
	if err != nil {
		if !isCaptureVerdict(err) {
			fmt.Fprintf(stderr, "✗ capture: %v\n", err)
			return 3
		}
		// nil created-set, deliberately: capture is what BUILDS the transition
		// data, so a capture rejection has no envelope and no trustworthy
		// creation list. The residue of a candidate that never got an envelope
		// stays on disk and is reported unattributed.
		recordVerdict(rt, candidateID, captureRejectionReason(err), nil, stderr)
		emitSubmitCaptureRejected(stdout, candidateID, err)
		return 1
	}

	// 5. Admission — re-validates against LIVE state, never trusting the
	// snapshot Capture just recorded. That includes CurrentEpoch: reusing the
	// SAME `epochs` map Capture's LeaseEpoch used would make the epoch check
	// trivially agree with itself and never catch anything — the whole point
	// of a fresh read here is to notice a lease released and re-granted (or
	// reclaimed) in the window between the lease check and this call.
	//
	// ‡ LOTO_GATE=off is the documented outage escape hatch (git-gate.md
	// Outcome 7, "the gate can never become the outage"). Checked HERE rather
	// than inside Admit because gate.Bypassed's contract puts the advisory and
	// the mandatory audit event on the caller — this is that caller.
	if gate.Bypassed() {
		code = submitGateBypass(rt, repoTop, env, owner, candidateID, proposal, writeSet, stdout, stderr)
		accepted = code == 0
		return code
	}

	decision, code := submitAdmit(rt, repoTop, env, owner, targets, proposal, stderr)
	if code != 0 {
		return code
	}
	if !decision.Accepted {
		recordVerdict(rt, candidateID, decision.Reason, env.CreatedPaths(), stderr)
		emitSubmitRejected(stdout, candidateID, decision)
		return 1
	}
	// 6. Accept: write refs/loto/candidates + refs/loto/proposals atomically,
	// convert each lease into a durable candidate claim.
	code = submitAccept(rt, repoTop, env, owner, candidateID, proposal, writeSet, true, stdout, stderr)
	accepted = code == 0
	return code
}

// recordVerdict appends the audit row `loto gate stats` counts. Best-effort
// with a ⚠ on failure: the verdict has already been rendered and acted on,
// and losing its breadcrumb must not change the exit code the caller sees.
// An empty reason means accepted.
//
// created is the rejected candidate's created write-set, each path bound to
// the blob it proposed — the attribution `loto sync` deletes residue by
// (loto-ovno.13). Pass nil wherever no envelope was ever built: attribution
// then fails closed, and sync reports the residue as unattributed rather than
// deleting a file nothing vouches for.
func recordVerdict(rt *runtime, candidateID string, reason gate.RejectionReason, created []gate.CreatedPath, stderr io.Writer) {
	if err := rt.Store.RecordAdmissionVerdict(rt.Ctx, rt.Agent.UUID, candidateID, reason, created); err != nil {
		fmt.Fprintf(stderr, "⚠ verdict not recorded id=%s: %v\n", candidateID, err)
	}
}

// submitAdmit is runSubmit step 5: read the live state admission judges
// against — current lease epochs and open violations — then call Admit. A
// non-zero code is an infrastructure failure the caller returns immediately;
// a Decision (accepted or not) is a verdict.
func submitAdmit(rt *runtime, repoTop string, env gate.Envelope, owner domain.AgentUUID, targets []domain.Target, proposal string, stderr io.Writer) (gate.Decision, int) {
	currentEpochs, err := currentPathEpochs(rt, targets, owner)
	if err != nil {
		fmt.Fprintf(stderr, "✗ admission: read current epochs: %v\n", err)
		return gate.Decision{}, 3
	}
	// Sensor pass, HERE and not earlier: the submitter's own leases are live
	// by now (step 1), so its own edits read as authorized rather than as
	// contamination. A scan failure is advisory — admission still runs
	// against whatever the store already holds, because a sensor that could
	// not read the tree must not become the outage the escape hatch exists to
	// prevent (git-gate.md Outcome 7).
	unresolved := submitViolationScan(rt, repoTop, stderr)
	// ‡ gate.IntegrationRef (the symbolic name), NOT the SHA resolved back at
	// step 2 (Codex #259 P1). AdmitParams.IntegrationRef is documented "re-read
	// fresh" — that is the whole basis of stale-preimage / stale-ancestry.
	// Handing it the frozen SHA makes both checks reread the same commit the
	// envelope was captured from, so neither verdict could ever fire. The
	// resolved SHA stays in use for the proposal base and the capture snapshot,
	// which DO want the point-in-time value.
	decision, err := gate.Admit(rt.Ctx, env, gate.AdmitParams{
		RepoTop: repoTop, PresentedProposalSHA: proposal,
		IntegrationRef: gate.IntegrationRef, CurrentEpoch: currentEpochs,
		UnresolvedViolations: unresolved,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ admission: %v\n", err)
		return gate.Decision{}, 3
	}
	return decision, 0
}

// submitViolationScan runs the whole-tree sensor pass and returns the open
// violations admission should judge against — path -> violation id.
//
// Every failure here degrades to "what the store already holds" with a ⚠
// row, never to an error: the scan makes admission's violation-intersect
// check SHARPER, and a submit that cannot run it is no worse off than a
// submit made before the sensor existed. Refusing instead would hand every
// broken-git-plumbing day a repo-wide submit outage.
func submitViolationScan(rt *runtime, repoTop string, stderr io.Writer) map[string]string {
	if res, err := runViolationScan(rt, repoTop); err != nil {
		fmt.Fprintf(stderr, "⚠ violation scan skipped: %v\n", err)
	} else if len(res.Recorded) > 0 {
		fmt.Fprintf(stderr, "⚠ violations recorded=%d by this scan\n", len(res.Recorded))
	}
	// Scoped to this checkout: a candidate proposes content from ONE tree,
	// and worktrees of a repo share the store (loto-nper). An unknown
	// checkout degrades like a read failure rather than guessing "primary",
	// which would intersect a linked worktree's submit against rows that
	// belong to a tree it cannot see.
	wt, err := gate.WorktreeID(rt.Ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ violation read skipped: %v\n", err)
		return nil
	}
	unresolved, err := rt.Store.UnresolvedViolationPaths(rt.Ctx, wt)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ violation read skipped: %v\n", err)
		return nil
	}
	return unresolved
}

// submitBuildProposal is runSubmit steps 2 and 3: resolve the integration ref
// this candidate is based on, then build the proposal commit with lane.Commit.
// The returned integrationRef is a point-in-time SHA — right for the proposal
// base and for the capture snapshot, and deliberately NOT what admission
// re-reads (see the Admit call site).
//
// ‡ On success the caller now owns refs/heads/loto/<candidateID>, which
// lane.Commit has already written — every non-accepting path from there must
// discard it (discardLaneRef).
func submitBuildProposal(rt *runtime, repoTop, bead, msg string, writeSet []string, stderr io.Writer) (integrationRef, candidateID, proposal string, code int) {
	integrationRef, err := gate.ResolveIntegrationRef(rt.Ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ resolve integration ref: %v\n", err)
		return "", "", "", 3
	}
	candidateID = gate.NewCandidateID()
	id := laneIdentity(rt.Agent)
	proposal, err = lane.Commit(rt.Ctx, lane.Opts{
		RepoTop:   repoTop,
		Base:      integrationRef,
		Ref:       candidateID,
		WriteSet:  writeSet,
		Message:   buildLaneMessage(msg, bead),
		Author:    id,
		Committer: id,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ lane commit: %v\n", err)
		return "", "", "", 3
	}
	return integrationRef, candidateID, proposal, 0
}

// submitGateBypass is the LOTO_GATE=off path: accept without an admission
// verdict, but never silently. gate.Bypassed's contract puts both obligations
// on the caller — print the loud advisory, and persist the audit event — and
// the event is not optional: a bypass that cannot be logged is exactly the
// silent normalization the escape hatch is designed to prevent, so a failed
// write refuses the submit rather than proceeding unrecorded.
func submitGateBypass(rt *runtime, repoTop string, env gate.Envelope, owner domain.AgentUUID, candidateID, proposal string, writeSet []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, gate.BypassAdvisory)
	if err := rt.Store.RecordGateBypass(rt.Ctx, string(owner), "loto submit: LOTO_GATE=off"); err != nil {
		fmt.Fprintf(stderr, "✗ record gate bypass: %v\n", err)
		return 3
	}
	// judged=false: a bypassed candidate never reached a verdict, and
	// recording one would let `loto gate stats` report the gate as having
	// accepted work it never looked at. The bypass has its own class.
	return submitAccept(rt, repoTop, env, owner, candidateID, proposal, writeSet, false, stdout, stderr)
}

// isCaptureVerdict separates gate.Capture's two candidate VERDICTS — the
// candidate is structurally invalid, and the operator fixes it by resubmitting
// — from every other error it can return, which is infrastructure (context
// cancelled, a git plumbing call that failed on permissions or a corrupt
// object store). Reporting the second kind as `candidate-rejected` would send
// the operator editing a candidate that was never the problem.
func isCaptureVerdict(err error) bool {
	return errors.Is(err, gate.ErrAncestryNotTree) || errors.Is(err, gate.ErrPathNotBlob)
}

// discardLaneRef drops refs/heads/loto/<candidateID>, the branch lane.Commit
// writes before the candidate has been judged. Best-effort and advisory-only:
// the submit already has its verdict, and failing to clean up a temporary ref
// must not turn a clean rejection into an infrastructure error. The proposal
// commit itself stays reachable while any candidate/proposal ref names it, so
// this only ever unreaches an object no accepted candidate depends on.
func discardLaneRef(ctx context.Context, repoTop, candidateID string, stderr io.Writer) {
	err := gate.UpdateRefsTx(ctx, repoTop, []gate.RefUpdate{{
		Verb: gate.VerbDelete, Ref: "refs/heads/loto/" + candidateID,
	}})
	if err != nil {
		fmt.Fprintf(stderr, "⚠ lane ref refs/heads/loto/%s not cleaned up: %v\n", candidateID, err)
	}
}

// submitLeaseCheck is runSubmit step 1 — "fail at the cheap end": no git
// plumbing, no envelope, no admission call until every target is confirmed
// held, live, exclusive. Reuses lane's own assertion (cmd_lane.go) — the
// same precondition, the same rejection shape. On success, returns the
// write-set and each target's lease epoch with code 0; otherwise the
// blocked/error report has already been written and code is the exit code
// runSubmit should return immediately.
func submitLeaseCheck(rt *runtime, ec domain.EvalContext, owner domain.AgentUUID, targets []domain.Target, stdout, stderr io.Writer) (writeSet []string, epochs map[string]int64, code int) {
	locks, err := rt.Store.LocksForOwnerAt(rt.Ctx, targets, owner)
	if err != nil {
		fmt.Fprintf(stderr, "✗ lease check: %v\n", err)
		return nil, nil, 3
	}
	if blocked := submitBlockedTargets(targets, locks, ec); len(blocked) > 0 {
		emitSubmitBlocked(stdout, blocked)
		return nil, nil, 1
	}

	epochs = make(map[string]int64, len(targets))
	writeSet = make([]string, len(targets))
	for i, t := range targets {
		epochs[t.Canonical] = locks[t.Canonical].Epoch
		writeSet[i] = t.Canonical
	}
	return writeSet, epochs, 0
}

// submitAccept is runSubmit step 6 — admission accepted: write
// refs/loto/candidates + refs/loto/proposals atomically, convert each lease
// into a durable candidate claim.
func submitAccept(rt *runtime, repoTop string, env gate.Envelope, owner domain.AgentUUID, candidateID, proposal string, writeSet []string, judged bool, stdout, stderr io.Writer) int {
	pid, src := stampPID()
	var procStart int64
	if src == pidDurable {
		procStart, _ = identityProcStart(pid)
	}
	submitter := domain.CandidateClaim{
		OwnerUUID: owner, SessionUUID: rt.SessionUUID, Host: rt.Host, PID: pid, ProcStart: procStart,
	}
	if submitBeforeAccept != nil {
		submitBeforeAccept(rt)
	}
	// The liveness probe is memoized for the same reason the lock path
	// memoizes it: the guard evaluates the predicate once per write-set path
	// inside a tx that must not stall on repeated process probes.
	envSHA, err := rt.Store.AcceptCandidate(rt.Ctx, repoTop, env, submitter, memoLiveProbe(rt.liveProbe()))
	if err != nil {
		// A lost lease is a REJECTION, not an internal failure: the gate
		// admitted this candidate on an epoch map read before AcceptCandidate
		// took the op-flock, and the store refused it on the live state
		// (loto-ovno.10). Same taxonomy class as an admission-time epoch
		// mismatch — the proposer's remedy is identical, re-lock and resubmit.
		var stale *store.LeaseRevalidationError
		if errors.As(err, &stale) {
			if judged {
				recordVerdict(rt, candidateID, gate.ReasonStaleLeaseEpoch, env.CreatedPaths(), stderr)
			}
			emitSubmitLeaseLost(stdout, candidateID, stale)
			return 1
		}
		fmt.Fprintf(stderr, "✗ accept: %v\n", err)
		return 3
	}
	if judged {
		recordVerdict(rt, candidateID, "", nil, stderr)
	}
	fmt.Fprintf(stdout, "✓ candidate id=%s envelope=%s proposal=%s files=%d\n", candidateID, envSHA, proposal, len(writeSet))
	return 0
}

// currentPathEpochs reads the LIVE epoch for each target, right now — the
// fresh half of the epoch check (see the call site's comment). A target
// absent from the read (the lease was released, not merely re-granted) reads
// as Go's map zero value, 0, which correctly never matches a captured epoch
// >= 1 — no special-casing needed for "the lease is just gone."
func currentPathEpochs(rt *runtime, targets []domain.Target, owner domain.AgentUUID) (map[string]int64, error) {
	locks, err := rt.Store.LocksForOwnerAt(rt.Ctx, targets, owner)
	if err != nil {
		return nil, err
	}
	out := make(map[string]int64, len(targets))
	for _, t := range targets {
		out[t.Canonical] = locks[t.Canonical].Epoch
	}
	return out, nil
}

// submitBlocked is one write-set path that failed the lease-check precondition.
type submitBlocked struct {
	Path   string
	Reason string
}

func submitBlockedTargets(targets []domain.Target, locks map[string]domain.LockRecord, ec domain.EvalContext) []submitBlocked {
	var blocked []submitBlocked
	for _, t := range targets {
		l, ok := locks[t.Canonical]
		switch {
		case !ok:
			blocked = append(blocked, submitBlocked{t.Canonical, "no-lock-held"})
		case ec.IsStale(l):
			blocked = append(blocked, submitBlocked{t.Canonical, lockStaleReason})
		case l.EffectiveMode() != domain.ModeExclusive:
			blocked = append(blocked, submitBlocked{t.Canonical, "lock-not-exclusive"})
		}
	}
	return blocked
}

func emitSubmitBlocked(w io.Writer, blocked []submitBlocked) {
	sort.Slice(blocked, func(i, j int) bool { return blocked[i].Path < blocked[j].Path })
	fmt.Fprintf(w, "✗ lease-check-failed count=%d\n", len(blocked))
	for _, b := range blocked {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", b.Path, b.Reason)
	}
	fmt.Fprintln(w, "```bash")
	fmt.Fprintln(w, `loto lock <target>... -t "why"`)
	fmt.Fprintln(w, "```")
}

// emitSubmitRejected renders an admission Decision per design.md: triage count
// first, one ✗ row naming the reason class, the actionable detail beneath it.
func emitSubmitRejected(w io.Writer, candidateID string, d gate.Decision) {
	fmt.Fprintf(w, "✗ candidate-rejected count=1 id=%s reason=%s\n", candidateID, d.Reason)
	fmt.Fprintf(w, "✗ %s\n", d.Detail)
}

// emitSubmitCaptureRejected renders a Capture-time failure with the same
// triage-first shape as an admission rejection — the caller should not have to
// learn two different failure grammars for "this candidate cannot be built"
// versus "this candidate was built but refused."
func emitSubmitCaptureRejected(w io.Writer, candidateID string, err error) {
	fmt.Fprintf(w, "✗ candidate-rejected count=1 id=%s reason=%s\n", candidateID, captureRejectionReason(err))
	fmt.Fprintf(w, "✗ %v\n", err)
}

// captureRejectionReason maps a Capture verdict onto the shared taxonomy, so
// the class the operator READS and the class `loto gate stats` COUNTS are the
// same string derived once. isCaptureVerdict has already established that err
// is one of the two verdicts; anything else reaching here is a bug upstream,
// and malformed-candidate is the honest floor for it.
func captureRejectionReason(err error) gate.RejectionReason {
	if errors.Is(err, gate.ErrAncestryNotTree) {
		return gate.ReasonStaleAncestry
	}
	return gate.ReasonMalformedCandidate
}

// emitSubmitLeaseLost renders a claim-time lease-revalidation refusal with the
// same triage-first shape as an admission rejection, one ✗ row per path.
func emitSubmitLeaseLost(w io.Writer, candidateID string, e *store.LeaseRevalidationError) {
	fmt.Fprintf(w, "✗ candidate-rejected count=%d id=%s reason=%s\n",
		len(e.Conflicts), candidateID, gate.ReasonStaleLeaseEpoch)
	for _, c := range e.Conflicts {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", c.Path, c.Reason)
	}
	fmt.Fprintln(w, "```bash")
	fmt.Fprintln(w, `loto lock <target>... -t "why"`)
	fmt.Fprintln(w, "```")
}

// submitBeforeAccept is a test seam, the twin of submitAfterLeaseCheck one
// step later: it fires after admission accepts and before AcceptCandidate
// takes the op-flock — the exact window loto-ovno.10 closes. Nil in
// production.
var submitBeforeAccept func(*runtime) //nolint:gochecknoglobals // test seam, production-nil

// identityProcStart is a package-var indirection so tests can stub the
// platform-specific proc-start reader without touching a real process table —
// mirrors buildLockRecords' own use of identity.ProcStart (cmd_lock.go).
var identityProcStart = identity.ProcStart

// submitAfterLeaseCheck is a test seam, mirroring cmd_lane.go's
// laneAfterPreAssert: fires after the lease check passes and epochs are read,
// before lane.Commit — letting a test perturb store state inside the window
// runSubmit itself cannot observe, to exercise an admission rejection
// end-to-end through the real CLI flow without needing promotion (a later
// bead) to actually advance refs/loto/integration. Nil in production.
var submitAfterLeaseCheck func(*runtime) //nolint:gochecknoglobals // test seam, production-nil
