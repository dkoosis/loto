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
)

func init() { register("submit", cmdSubmit) } //nolint:gochecknoinits // command registry pattern

// submitUsageHead is the point-of-use teaching surface (loto-5rwc).
const submitUsageHead = `usage: loto submit <file> [<file>...] --bead <id> [-m "<msg>"]

Package your held locks' current edits into a candidate for the git-gate
pipeline: lease check -> lane.Commit -> envelope capture -> admission.

On accept: writes refs/loto/candidates/<id> + refs/loto/proposals/<id>, and
converts each lease into a durable candidate claim. On reject: nothing is
written to git or the store — fix the reported reason and resubmit. This is
the first point where the whole front half of the gate is observable in one
command (loto-ovno.5).

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
	ec := domain.EvalContext{Now: time.Now(), Live: rt.liveProbe()}

	// 1. LEASE CHECK — "fail at the cheap end": no git plumbing, no envelope,
	// no admission call until every target is confirmed held, live, exclusive.
	writeSet, epochs, code := submitLeaseCheck(rt, ec, owner, targets, stdout, stderr)
	if code != 0 {
		return code
	}

	if submitAfterLeaseCheck != nil {
		submitAfterLeaseCheck(rt)
	}

	// 2. Resolve the integration ref (bootstraps to HEAD on first use).
	integrationRef, err := gate.ResolveIntegrationRef(rt.Ctx, repoTop)
	if err != nil {
		fmt.Fprintf(stderr, "✗ resolve integration ref: %v\n", err)
		return 3
	}

	// 3. lane.Commit — private index, exact write-set, no HEAD move.
	candidateID := gate.NewCandidateID()
	id := laneIdentity(rt.Agent)
	proposal, err := lane.Commit(rt.Ctx, lane.Opts{
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
		return 3
	}

	// 4. Envelope capture. A Capture error (ErrAncestryNotTree, ErrPathNotBlob)
	// is a real "this candidate is already invalid" verdict, not plumbing
	// failure — render it as a rejection, not an infra error, so the operator
	// sees the same triage-first shape admission's own rejections use.
	env, err := gate.Capture(rt.Ctx, gate.CaptureParams{
		RepoTop: repoTop, IntegrationRef: integrationRef, ProposalSHA: proposal,
		Base: integrationRef, WriteSet: writeSet, CandidateID: candidateID,
		Agent: owner, Session: rt.SessionUUID, BeadID: bead, LeaseEpoch: epochs,
	})
	if err != nil {
		emitSubmitCaptureRejected(stdout, candidateID, err)
		return 1
	}

	// 5. Admission — re-validates against LIVE state, never trusting the
	// snapshot Capture just recorded. That includes CurrentEpoch: reusing the
	// SAME `epochs` map Capture's LeaseEpoch used would make the epoch check
	// trivially agree with itself and never catch anything — the whole point
	// of a fresh read here is to notice a lease released and re-granted (or
	// reclaimed) in the window between the lease check and this call.
	currentEpochs, err := currentPathEpochs(rt, targets, owner)
	if err != nil {
		fmt.Fprintf(stderr, "✗ admission: read current epochs: %v\n", err)
		return 3
	}
	decision, err := gate.Admit(rt.Ctx, env, gate.AdmitParams{
		RepoTop: repoTop, PresentedProposalSHA: proposal,
		IntegrationRef: integrationRef, CurrentEpoch: currentEpochs,
	})
	if err != nil {
		fmt.Fprintf(stderr, "✗ admission: %v\n", err)
		return 3
	}
	if !decision.Accepted {
		emitSubmitRejected(stdout, candidateID, decision)
		return 1
	}

	// 6. Accept: write refs/loto/candidates + refs/loto/proposals atomically,
	// convert each lease into a durable candidate claim.
	return submitAccept(rt, repoTop, env, owner, candidateID, proposal, writeSet, stdout, stderr)
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
func submitAccept(rt *runtime, repoTop string, env gate.Envelope, owner domain.AgentUUID, candidateID, proposal string, writeSet []string, stdout, stderr io.Writer) int {
	pid, src := stampPID()
	var procStart int64
	if src == pidDurable {
		procStart, _ = identityProcStart(pid)
	}
	submitter := domain.CandidateClaim{
		OwnerUUID: owner, SessionUUID: rt.SessionUUID, Host: rt.Host, PID: pid, ProcStart: procStart,
	}
	envSHA, err := rt.Store.AcceptCandidate(rt.Ctx, repoTop, env, submitter)
	if err != nil {
		fmt.Fprintf(stderr, "✗ accept: %v\n", err)
		return 3
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
	reason := "capture-failed"
	switch {
	case errors.Is(err, gate.ErrAncestryNotTree):
		reason = "stale-ancestry"
	case errors.Is(err, gate.ErrPathNotBlob):
		reason = "malformed-candidate"
	}
	fmt.Fprintf(w, "✗ candidate-rejected count=1 id=%s reason=%s\n", candidateID, reason)
	fmt.Fprintf(w, "✗ %v\n", err)
}

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
