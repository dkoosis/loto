//go:build unix

package gate

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"loto/internal/domain"
	"loto/internal/lane"
)

// ClaimReleaser is the store-side retire hook: once a candidate's refs are
// gone, its durable claims stop blocking overlapping acquisitions.
// *store.Store satisfies it as written, which is why this package still has no
// store dependency.
type ClaimReleaser interface {
	ReleaseCandidateClaims(ctx context.Context, candidateID string) error
}

// PromoteParams is Promote's input. RepoTop, VerifyCmd, and Claims are
// required; the rest take defaults.
type PromoteParams struct {
	RepoTop string
	// VerifyCmd is the gate-owned invariant command run against the
	// prospective chain tip. A parameter, not config, until the config bead
	// gives loto somewhere outside any candidate tree to keep it.
	VerifyCmd []string
	MaxBatch  int // 0 → defaultMaxBatch
	MaxRounds int // 0 → defaultMaxRounds
	Claims    ClaimReleaser
	// Live decides whether a crashed pusher's promoting claim may be
	// reclaimed. Nil never reclaims — the same rule every other liveness
	// verdict in loto follows.
	Live domain.HolderLiveProbe
	// Host/PID/ProcStart identify THIS pusher, stamped into the promoting
	// claim so a later pusher can decide whether this process is still alive.
	// Supplied by the caller: gate stays identity-free.
	Host      string
	PID       int
	ProcStart int64
}

// OutcomeClass is the promotion taxonomy. Every candidate a round touches
// leaves with exactly one of these.
type OutcomeClass string

const (
	// OutcomePromoted: the candidate's commit is on refs/loto/integration.
	OutcomePromoted OutcomeClass = "promoted"
	// OutcomeStalePreimage / OutcomeStaleAncestry: the candidate no longer
	// applies to integration as it stands. Skipped, NOT rejected — its refs
	// and claims are untouched and a later run may still promote it.
	OutcomeStalePreimage OutcomeClass = "stale-preimage"
	OutcomeStaleAncestry OutcomeClass = "stale-ancestry"
	// OutcomeVerifyRed: the candidate alone fails verify. The one outcome
	// here that rejects — refs deleted, claims released — because a red
	// candidate left in the queue wedges every future promotion.
	OutcomeVerifyRed OutcomeClass = "verify-red"
	// OutcomeVerifyInfra: verify could not reach a verdict. Never rejects.
	OutcomeVerifyInfra OutcomeClass = "verify-infrastructure"
	// OutcomePromotionRace: integration moved, or a ref this batch depended
	// on changed, between phase 1 and phase 3. Requeued.
	OutcomePromotionRace OutcomeClass = "promotion-race"
)

// CandidateOutcome is one candidate's result, carrying the attribution a
// rejection needs to name someone.
type CandidateOutcome struct {
	CandidateID string
	Class       OutcomeClass
	Agent       domain.AgentUUID
	Session     domain.SessionUUID
	BeadID      string
	Detail      string
}

// PromoteResult is what one Promote call did. Outcomes are sorted by
// CandidateID; rendering (glyphs, triage counts) belongs to the CLI bead.
type PromoteResult struct {
	Integration string
	Outcomes    []CandidateOutcome
	// Verifies is one entry per phase-2 verify, in the order they ran.
	Verifies []VerifyRun
}

// VerifyRun is one phase-2 verify: which batch it judged, how many candidates
// rode in it, and how long lane.Verify itself took.
//
// ‡ Per-run, never summed. The loto-ovno.1 budget (p50 ≤ 1.5s, p95 ≤ 3s) is
// stated over ONE verify, and batching changes how many of these there are,
// never how long one takes — so a caller grading the budget reads Duration
// per entry and never a total across the call.
type VerifyRun struct {
	BatchID    string
	Candidates int
	Duration   time.Duration
	Passed     bool
	// Infra marks a verify that could not reach a verdict. Its Duration is
	// wall time spent, not a measurement of the invariant command, and grading
	// the budget must skip it.
	Infra bool
	// Tree names which checkout ran this verify (reuse / recut / fresh), and
	// TreeReason says why it was not reuse. Carried through to the CLI row: a
	// Duration back at pre-loto-moqi levels means nothing on its own, and this
	// is what says whether the reuse tree stopped working.
	Tree       lane.VerifyTreeMode
	TreeReason string
}

// timedVerify runs the phase-2 verify and records how long it took. The one
// place lane.Verify is called from promotion, so no path can land a verify
// that the result does not account for.
func timedVerify(ctx context.Context, p PromoteParams, b *batch, rec *[]VerifyRun) (lane.VerifyResult, error) {
	start := time.Now()
	vr, err := lane.Verify(ctx, p.RepoTop, b.chainTip, p.VerifyCmd)
	*rec = append(*rec, VerifyRun{
		BatchID:    b.id,
		Candidates: len(b.members),
		Duration:   time.Since(start),
		Passed:     err == nil && vr.Passed,
		Infra:      err != nil,
		Tree:       vr.Tree,
		TreeReason: vr.TreeReason,
	})
	return vr, err
}

const (
	defaultMaxBatch = 3
	// ‡ Three, not eight. The red-batch fallback costs n+1 verifies, so a red
	// batch of eight wedges promotion for nine verify-lengths. Batching
	// reduces the NUMBER of verifies, never the latency of one — the
	// loto-ovno.1 budget is per-verify and amortization is not compliance
	// with it.
	//
	// ‡ Both values STAND on a measurement, 2026-09-05 (loto-3is6). 22
	// single-candidate promotions driven through `loto promote` on a scratch
	// clone of this repo, invariant command `true`: p50 2.32s, p95 3.96s per
	// phase-2 verify — already over the budget with a command that does
	// nothing. The whole cost is lane.Verify's `git worktree add --detach` +
	// `worktree remove` on a 327-file tree (add alone measured 3.0–7.1s on the
	// same box, load average 28–37 from nine concurrent agent sessions).
	// Batches of two cost 3.7–5.8s for ONE verify, the same per-verify range
	// as a batch of one — the measured form of the paragraph above. So the
	// budget miss was in the verify harness, and the fix was a cheaper cut,
	// NOT a bigger batch: raising MaxBatch would have left one verify exactly
	// as slow and made the red-batch fallback's n+1 worse.
	//
	// ‡ That cut is now paid once per repo, not once per verify: lane.Verify
	// reuses one detached worktree under the git common dir (loto-moqi). The
	// reasoning above is unchanged by it — batching still reduces the number
	// of verifies and never the latency of one.
	defaultMaxRounds = 4
)

// promoteBeforePhase3Fn runs between verify and the phase-3 transaction. Nil
// in production; the seam a test uses to move integration underneath a batch
// deterministically, matching the repo's acquireOpFlockFn/commitTxFn pattern.
//
// ‡ It is a mutable package var, which is why no test in this package may call
// t.Parallel.
var promoteBeforePhase3Fn func()

var (
	// errPromoteInput is a caller bug — a required parameter left blank.
	errPromoteInput = errors.New("gate: invalid Promote input")
	// errPromoteRace is internal: the phase-3 CAS lost. Never surfaces to the
	// caller as an error, only as OutcomePromotionRace.
	errPromoteRace = errors.New("gate: integration moved under the batch")
	// errBadPromotingClaim marks a promoting ref whose trailers do not parse.
	// Its batch is treated as live and left alone — see reclaimDeadPromotions.
	errBadPromotingClaim = errors.New("gate: unreadable promoting claim")
)

// Promote drains accepted candidates onto refs/loto/integration in bounded
// rounds, in the pusher's own process — no daemon.
//
// Each round is three phases:
//
//  1. Under the promotion flock: snapshot integration, reclaim any dead
//     pusher's promoting refs, select eligible candidates in candidate-ID
//     order, revalidate every preimage and ancestry against the snapshot,
//     build a prospective chain of synthetic commits, and claim the batch.
//  2. With NO lock held: verify the chain tip. Admission keeps running.
//  3. Under the flock again: one update-ref transaction, every line CAS'd
//     against what phase 1 read.
//
// ‡ Phase 3's correctness rides the ref CAS, not the flock. The integration
// update carries OldSHA = the phase-1 snapshot, so any interleaving at all —
// integration advanced, a candidate retired, a claim reclaimed — fails the
// whole transaction atomically and the round requeues. The flock only stops
// two pushers from doing the same expensive work twice.
func Promote(ctx context.Context, p PromoteParams) (PromoteResult, error) {
	switch {
	case p.RepoTop == "":
		return PromoteResult{}, fmt.Errorf("%w: RepoTop", errPromoteInput)
	case len(p.VerifyCmd) == 0 || p.VerifyCmd[0] == "":
		return PromoteResult{}, fmt.Errorf("%w: VerifyCmd", errPromoteInput)
	case p.Claims == nil:
		return PromoteResult{}, fmt.Errorf("%w: Claims", errPromoteInput)
	}
	if p.MaxBatch <= 0 {
		p.MaxBatch = defaultMaxBatch
	}
	if p.MaxRounds <= 0 {
		p.MaxRounds = defaultMaxRounds
	}

	var res PromoteResult
	for range p.MaxRounds {
		batch, outcomes, err := promotePhase1(ctx, p)
		res.Outcomes = append(res.Outcomes, outcomes...)
		if err != nil {
			return finish(ctx, res, p), err
		}
		if batch == nil {
			break // nothing eligible; the queue is drained for now
		}

		done, stop, err := promoteVerifyAndCommit(ctx, p, batch, &res.Verifies)
		res.Outcomes = append(res.Outcomes, done...)
		if err != nil {
			return finish(ctx, res, p), err
		}
		if stop {
			break
		}
	}
	return finish(ctx, res, p), nil
}

// finish stamps the current integration SHA and sorts the outcomes.
func finish(ctx context.Context, res PromoteResult, p PromoteParams) PromoteResult {
	if sha, err := ResolveIntegrationRef(ctx, p.RepoTop); err == nil {
		res.Integration = sha
	}
	sort.SliceStable(res.Outcomes, func(i, j int) bool {
		return res.Outcomes[i].CandidateID < res.Outcomes[j].CandidateID
	})
	return res
}

// promoteVerifyAndCommit runs phases 2 and 3 for one claimed batch, including
// the red-batch fallback. stop reports that the round loop should end
// (infrastructure trouble, which is environmental and not worth a tight
// retry).
func promoteVerifyAndCommit(ctx context.Context, p PromoteParams, b *batch, rec *[]VerifyRun) (outcomes []CandidateOutcome, stop bool, err error) {
	vr, verr := timedVerify(ctx, p, b, rec)
	if verr != nil {
		// Could not reach a verdict. Release the claim, leave every candidate
		// exactly as it was, and stop: infra failures do not improve on retry.
		if derr := releaseBatch(ctx, p.RepoTop, b); derr != nil {
			return b.classifyAll(OutcomeVerifyInfra, verr.Error()), true, derr
		}
		return b.classifyAll(OutcomeVerifyInfra, verr.Error()), true, nil
	}
	if !vr.Passed {
		if derr := releaseBatch(ctx, p.RepoTop, b); derr != nil {
			return nil, true, derr
		}
		return redBatchSweep(ctx, p, b, rec)
	}

	if promoteBeforePhase3Fn != nil {
		promoteBeforePhase3Fn()
	}

	switch err := promotePhase3(ctx, p, b); {
	case err == nil:
		return b.promoted(ctx, p), false, nil
	case errors.Is(err, errPromoteRace):
		// The CAS lost. Drop the claim and let the next round rebuild against
		// whatever integration is now.
		if derr := releaseBatch(ctx, p.RepoTop, b); derr != nil {
			return b.classifyAll(OutcomePromotionRace, err.Error()), true, derr
		}
		return b.classifyAll(OutcomePromotionRace, err.Error()), false, nil
	default:
		return nil, true, err
	}
}

// redBatchSweep is the red-batch rule: the batch verified red, so re-run the
// whole three-phase flow for each member ALONE, in the original deterministic
// order.
//
// ‡ No bisection. A batch of n costs at most n+1 verifies, which is the price
// of attributing a red to exactly one candidate instead of punishing the batch
// — and a candidate that is red on its own must be rejected, or it wedges
// every future promotion behind it.
func redBatchSweep(ctx context.Context, p PromoteParams, red *batch, rec *[]VerifyRun) (outcomes []CandidateOutcome, stop bool, err error) {
	solo := p
	solo.MaxBatch = 1
	for i := range red.members {
		outs, stop, err := promoteSolo(ctx, p, solo, red.members[i].candidateID, rec)
		outcomes = append(outcomes, outs...)
		if err != nil || stop {
			return outcomes, true, err
		}
	}
	return outcomes, false, nil
}

// promoteSolo runs the full three-phase flow for one candidate on a fresh
// snapshot. stop reports that the sweep should abandon the remaining
// candidates — infrastructure trouble is environmental, not per-candidate.
func promoteSolo(ctx context.Context, p, solo PromoteParams, id string, rec *[]VerifyRun) (outcomes []CandidateOutcome, stop bool, err error) {
	one, outs, perr := promotePhase1For(ctx, solo, id)
	outcomes = append(outcomes, outs...)
	if perr != nil {
		return outcomes, true, perr
	}
	if one == nil {
		return outcomes, false, nil // went stale or was taken while we verified
	}
	vr, verr := timedVerify(ctx, p, one, rec)
	if verr != nil {
		if derr := releaseBatch(ctx, p.RepoTop, one); derr != nil {
			return outcomes, true, derr
		}
		return append(outcomes, one.classifyAll(OutcomeVerifyInfra, verr.Error())...), true, nil
	}
	if !vr.Passed {
		rejected, rerr := rejectSolo(ctx, p, one, vr.Output)
		if rerr != nil {
			return outcomes, true, rerr
		}
		return append(outcomes, rejected...), false, nil
	}
	switch err := promotePhase3(ctx, p, one); {
	case err == nil:
		return append(outcomes, one.promoted(ctx, p)...), false, nil
	case errors.Is(err, errPromoteRace):
		if derr := releaseBatch(ctx, p.RepoTop, one); derr != nil {
			return outcomes, true, derr
		}
		return append(outcomes, one.classifyAll(OutcomePromotionRace, err.Error())...), false, nil
	default:
		return outcomes, true, err
	}
}

// rejectSolo retires a candidate that failed verify on its own: candidate ref,
// proposal ref, and the solo promoting ref go in one CAS'd transaction, then
// the claims are released.
//
// ‡ This is the only path in promotion that destroys a candidate, and it fires
// only on a positive Passed=false from a chain containing that candidate
// alone. An infrastructure error never reaches here.
func rejectSolo(ctx context.Context, p PromoteParams, b *batch, verifyOutput string) ([]CandidateOutcome, error) {
	m := b.members[0]
	updates := []RefUpdate{
		{Verb: VerbDelete, Ref: candidateRef(m.candidateID), OldSHA: m.envelopeSHA},
		{Verb: VerbDelete, Ref: proposalRef(m.candidateID), OldSHA: m.proposalSHA},
		{Verb: VerbDelete, Ref: promotingRef(b.id), OldSHA: b.chainTip},
	}
	if err := UpdateRefsTx(ctx, p.RepoTop, updates); err != nil {
		return nil, fmt.Errorf("gate: retire red candidate %s: %w", m.candidateID, err)
	}
	detail := trimVerifyOutput(verifyOutput)
	if err := p.Claims.ReleaseCandidateClaims(ctx, m.candidateID); err != nil {
		// The refs are gone, which is what unblocks the queue. The rows left
		// behind match doctor's residue rule exactly (a claim whose candidate
		// ref is absent), so `loto doctor --repair` clears them.
		detail += " (claim release failed: " + err.Error() + ")"
	}
	return []CandidateOutcome{m.outcome(OutcomeVerifyRed, detail)}, nil
}

// trimVerifyOutput keeps the tail of a verify failure — the part naming what
// broke — bounded so an outcome row stays readable.
func trimVerifyOutput(out string) string {
	const limit = 2000
	out = strings.TrimSpace(out)
	if len(out) <= limit {
		return out
	}
	return "…" + out[len(out)-limit:]
}

// member is one candidate inside a claimed batch, with the ref SHAs phase 3's
// CAS deletes are guarded on.
type member struct {
	candidateID string
	envelopeSHA string
	proposalSHA string
	env         Envelope
}

func (m member) outcome(class OutcomeClass, detail string) CandidateOutcome {
	return CandidateOutcome{
		CandidateID: m.candidateID,
		Class:       class,
		Agent:       m.env.AgentUUID,
		Session:     m.env.SessionUUID,
		BeadID:      m.env.BeadID,
		Detail:      detail,
	}
}

// batch is one claimed promotion attempt: the members, the chain built for
// them, and the snapshot the chain was built on.
type batch struct {
	id       string
	snapshot string
	chainTip string
	members  []member
}

func (b *batch) classifyAll(class OutcomeClass, detail string) []CandidateOutcome {
	outs := make([]CandidateOutcome, 0, len(b.members))
	for i := range b.members {
		outs = append(outs, b.members[i].outcome(class, detail))
	}
	return outs
}

// promoted releases each promoted candidate's claims and returns the outcomes.
//
// ‡ Refs first, rows second (the transaction already happened). A release
// error is reported in Detail and never fails a promotion that has already
// landed — the leftover rows are exactly doctor's residue shape.
func (b *batch) promoted(ctx context.Context, p PromoteParams) []CandidateOutcome {
	outs := make([]CandidateOutcome, 0, len(b.members))
	for i := range b.members {
		m := &b.members[i]
		detail := ""
		if err := p.Claims.ReleaseCandidateClaims(ctx, m.candidateID); err != nil {
			detail = "claim release failed: " + err.Error()
		}
		outs = append(outs, m.outcome(OutcomePromoted, detail))
	}
	return outs
}

// releaseBatch drops a promoting claim without touching its candidates. Used
// on every path that abandons a batch: infra error, verify-red (before the
// solo sweep), and a lost CAS.
func releaseBatch(ctx context.Context, repoTop string, b *batch) error {
	err := UpdateRefsTx(ctx, repoTop, []RefUpdate{
		{Verb: VerbDelete, Ref: promotingRef(b.id), OldSHA: b.chainTip},
	})
	if err != nil {
		return fmt.Errorf("gate: release promoting claim %s: %w", b.id, err)
	}
	return nil
}

// promotePhase1 selects and claims a batch under the promotion flock.
func promotePhase1(ctx context.Context, p PromoteParams) (*batch, []CandidateOutcome, error) {
	return promotePhase1For(ctx, p, "")
}

// promotePhase1For is phase 1, optionally restricted to a single candidate id
// (the red-batch sweep's solo round).
func promotePhase1For(ctx context.Context, p PromoteParams, only string) (*batch, []CandidateOutcome, error) {
	fl, err := acquirePromoteFlock(ctx, p.RepoTop)
	if err != nil {
		return nil, nil, err
	}
	defer fl.release()

	snapshot, err := ResolveIntegrationRef(ctx, p.RepoTop)
	if err != nil {
		return nil, nil, err
	}
	claimed, err := reclaimDeadPromotions(ctx, p)
	if err != nil {
		return nil, nil, err
	}
	members, outcomes, err := selectCandidates(ctx, p, snapshot, claimed, only)
	if err != nil || len(members) == 0 {
		return nil, outcomes, err
	}

	b := &batch{id: newBatchID(), snapshot: snapshot, members: members}
	tip, err := buildChain(ctx, p, b)
	if err != nil {
		return nil, outcomes, err
	}
	b.chainTip = tip
	// Claim the batch before releasing the flock: the ref is what keeps the
	// chain reachable through the unlocked verify, and what tells a peer this
	// work is already in flight.
	if err := UpdateRefsTx(ctx, p.RepoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: promotingRef(b.id), NewSHA: tip},
	}); err != nil {
		return nil, outcomes, fmt.Errorf("gate: claim batch %s: %w", b.id, err)
	}
	return b, outcomes, nil
}

// selectCandidates walks the candidate refs in id order and takes the ones
// that still apply to the snapshot, up to MaxBatch.
func selectCandidates(ctx context.Context, p PromoteParams, snapshot string, claimed map[string]bool, only string) ([]member, []CandidateOutcome, error) {
	refs, err := listRefsWithSHAs(ctx, p.RepoTop, candidateRefPrefix)
	if err != nil {
		return nil, nil, err
	}
	proposals, err := listRefsWithSHAs(ctx, p.RepoTop, "refs/loto/proposals/")
	if err != nil {
		return nil, nil, err
	}
	ids := make([]string, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	sel := selection{taken: map[string]bool{}}
	for _, id := range ids {
		if len(sel.members) >= p.MaxBatch {
			break
		}
		if (only != "" && id != only) || claimed[id] {
			continue // not this round's, or another pusher's live batch owns it
		}
		m, ok := loadMember(ctx, p.RepoTop, id, refs[id], proposals)
		if !ok || overlaps(sel.taken, m.env.WriteSet) {
			continue
		}
		if err := sel.consider(ctx, p.RepoTop, snapshot, m); err != nil {
			return nil, sel.outcomes, err
		}
	}
	return sel.members, sel.outcomes, nil
}

// selection accumulates one round's batch: the members taken, the write-set
// paths they have spoken for, and the outcomes of the ones passed over.
type selection struct {
	members  []member
	outcomes []CandidateOutcome
	taken    map[string]bool
}

// consider takes m into the batch unless it has gone stale against the
// snapshot, in which case it is SKIPPED — refs and claims untouched, so a
// later round may still promote it.
func (s *selection) consider(ctx context.Context, repoTop, snapshot string, m member) error {
	stale, err := classifyStale(ctx, repoTop, m.env, snapshot)
	if err != nil {
		return err
	}
	if stale != nil {
		s.outcomes = append(s.outcomes, m.outcome(stale.class, stale.detail))
		return nil
	}
	for _, path := range m.env.WriteSet {
		s.taken[path] = true
	}
	s.members = append(s.members, m)
	return nil
}

// loadMember reads one candidate's envelope and proposal ref. Not-ok means the
// candidate is not promotable right now and the caller should move on.
//
// ‡ Both failure modes are deliberately silent skips, not errors. A candidate
// ref whose envelope will not decode is malformed — retiring it belongs to
// loto-ovno.8/.11 — and a candidate with no proposal ref is half-written
// acceptance, which doctor's residue rule already owns. Neither should stop the
// rest of the queue from draining.
func loadMember(ctx context.Context, repoTop, id, envelopeSHA string, proposals map[string]string) (member, bool) {
	env, err := ReadBlob(ctx, repoTop, envelopeSHA)
	if err != nil {
		return member{}, false
	}
	proposalSHA, ok := proposals[id]
	if !ok {
		return member{}, false
	}
	return member{candidateID: id, envelopeSHA: envelopeSHA, proposalSHA: proposalSHA, env: env}, true
}

type staleVerdict struct {
	class  OutcomeClass
	detail string
}

// classifyStale re-runs admission's own freshness checks against the snapshot.
// Same functions admission calls — a second definition of "still applies"
// would be a second place for the two to disagree.
func classifyStale(ctx context.Context, repoTop string, env Envelope, snapshot string) (*staleVerdict, error) {
	d, err := checkTransitionsCurrent(ctx, repoTop, env, snapshot)
	if err != nil {
		return nil, err
	}
	if !d.Accepted {
		return &staleVerdict{class: OutcomeStalePreimage, detail: d.Detail}, nil
	}
	d, err = checkAncestryCurrent(ctx, repoTop, env, snapshot)
	if err != nil {
		return nil, err
	}
	if !d.Accepted {
		return &staleVerdict{class: OutcomeStaleAncestry, detail: d.Detail}, nil
	}
	return nil, nil //nolint:nilnil // (nil, nil) is "not stale"; a verdict, not an error
}

// overlaps reports whether any of writeSet is already spoken for in this batch.
//
// ‡ Lease acquisition already blocks overlap with unresolved claims, so a hit
// here should be unreachable — asserted rather than assumed, because
// disjointness is what makes the chain's snapshot-relative preimage checks
// sufficient. First by candidate id wins.
func overlaps(taken map[string]bool, writeSet []string) bool {
	for _, p := range writeSet {
		if taken[p] {
			return true
		}
	}
	return false
}

// reclaimDeadPromotions sweeps refs/loto/promoting/*, deleting the claims of
// pushers that are provably gone and reporting which candidates the surviving
// claims own.
//
// ‡ Liveness is domain.EvalContext.CandidateClaimIsDead and nothing else: a
// nil probe or an UNKNOWN verdict both leave the claim standing. Reclaiming on
// an absence of evidence would let two pushers build chains on the same
// candidates.
func reclaimDeadPromotions(ctx context.Context, p PromoteParams) (map[string]bool, error) {
	refs, err := listRefsWithSHAs(ctx, p.RepoTop, promotingRefPrefix)
	if err != nil {
		return nil, err
	}
	claimed := map[string]bool{}
	ec := domain.EvalContext{Now: time.Now(), Live: p.Live}
	for batchID, tip := range refs {
		cl, err := readPromotionClaim(ctx, p.RepoTop, tip)
		if err != nil {
			if !errors.Is(err, errBadPromotingClaim) {
				// Not a decode failure — a git-read error, a cancelled
				// context. Fail-safe-continue is the wrong response to a
				// reason that isn't "we can't parse this claim"; surface it.
				return nil, fmt.Errorf("gate: read promoting claim %s: %w", batchID, err)
			}
			// Undecodable claim: fail SAFE. Mark every candidate we could
			// still read (readPromotionClaim parses Batch-Candidates before
			// it can fail on PID/ProcStart) as spoken for, and leave the ref
			// alone — deleting a claim we cannot read is how a live peer's
			// batch gets stolen; a residue sweep, not selectCandidates, is
			// what should eventually clean up a claim nobody can read.
			for _, id := range cl.candidates {
				claimed[id] = true
			}
			continue
		}
		if !ec.CandidateClaimIsDead(domain.CandidateClaim{
			// OwnerUUID stays empty: the probe reads only host/pid/proc-start,
			// and inventing an owner would imply an identity gate does not have.
			Host:      cl.host,
			PID:       cl.pid,
			ProcStart: cl.procStart,
		}) {
			for _, id := range cl.candidates {
				claimed[id] = true
			}
			continue
		}
		if err := UpdateRefsTx(ctx, p.RepoTop, []RefUpdate{
			{Verb: VerbDelete, Ref: promotingRef(batchID), OldSHA: tip},
		}); err != nil {
			return nil, fmt.Errorf("gate: reclaim promoting claim %s: %w", batchID, err)
		}
	}
	return claimed, nil
}

// promotionClaim is what a promoting ref's tip commit says about the pusher
// that built it, read back out of the commit message trailers.
type promotionClaim struct {
	host       string
	pid        int
	procStart  int64
	candidates []string
}

// Trailer keys on the chain tip. The per-candidate attribution trailers below
// are on every chain commit; these batch-level ones ride on the tip only,
// where the promoting ref points.
const (
	trailerBatch      = "Batch"
	trailerCandidates = "Batch-Candidates"
	trailerHost       = "Promoter-Host"
	trailerPID        = "Promoter-PID"
	trailerProcStart  = "Promoter-ProcStart"
)

// readPromotionClaim parses the candidate list FIRST, unconditionally, before
// the PID/ProcStart fields that can fail to parse — so a caller that gets an
// errBadPromotingClaim back still receives every candidate id the message
// named. reclaimDeadPromotions' fail-safe promise ("treat an unreadable
// claim's candidates as spoken for") depends on that: the zero-value struct
// this function used to return alongside an error carried no candidates at
// all, so the fail-safe path had nothing to mark claimed (loto-fxja).
func readPromotionClaim(ctx context.Context, repoTop, tip string) (promotionClaim, error) {
	g := gitRunner{repoTop: repoTop}
	msg, err := g.run(ctx, "log", "-1", "--format=%B", tip)
	if err != nil {
		return promotionClaim{}, err
	}
	tr := parseTrailers(msg)

	var cl promotionClaim
	for id := range strings.SplitSeq(tr[trailerCandidates], ",") {
		if id = strings.TrimSpace(id); id != "" {
			cl.candidates = append(cl.candidates, id)
		}
	}

	pid, err := strconv.Atoi(tr[trailerPID])
	if err != nil {
		return cl, fmt.Errorf("%w: %s: pid: %w", errBadPromotingClaim, tip, err)
	}
	start, err := strconv.ParseInt(tr[trailerProcStart], 10, 64)
	if err != nil {
		return cl, fmt.Errorf("%w: %s: proc-start: %w", errBadPromotingClaim, tip, err)
	}
	cl.host, cl.pid, cl.procStart = tr[trailerHost], pid, start
	if len(cl.candidates) == 0 {
		return cl, fmt.Errorf("%w: %s: names no candidates", errBadPromotingClaim, tip)
	}
	return cl, nil
}

// parseTrailers reads "Key: value" lines out of a commit message. Last
// occurrence wins, which is git's own trailer semantics.
func parseTrailers(msg string) map[string]string {
	out := map[string]string{}
	for line := range strings.SplitSeq(msg, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ": ")
		if !ok {
			continue
		}
		out[k] = strings.TrimSpace(v)
	}
	return out
}

// buildChain applies each member's Transitions onto a running base, producing
// one synthetic commit per candidate, and returns the tip.
//
// ‡ Transitions are applied, NOT the proposal commit cherry-picked. The
// transition set is the unit the CAS theorem is stated over — "promotion
// changes exactly the write-set paths and preserves all other integration
// state" — whereas replaying a proposal's diff could smuggle in differences
// from the base it happened to be built on.
func buildChain(ctx context.Context, p PromoteParams, b *batch) (string, error) {
	tmp, err := os.MkdirTemp("", "loto-promote-")
	if err != nil {
		return "", fmt.Errorf("gate: promote tempdir: %w", err)
	}
	defer os.RemoveAll(tmp)
	idx := filepath.Join(tmp, "index")

	base := b.snapshot
	for i := range b.members {
		m := &b.members[i]
		tree, err := applyTransitions(ctx, p.RepoTop, idx, base, m.env.Transitions)
		if err != nil {
			return "", err
		}
		last := i == len(b.members)-1
		commit, err := commitTree(ctx, p, tree, base, *m, b, last)
		if err != nil {
			return "", err
		}
		base = commit
	}
	return base, nil
}

// applyTransitions writes one candidate's paths into a scratch index seeded
// from base's tree and returns the resulting tree SHA.
//
// ‡ A deleted path (Result == nil) is expressed as mode 0 with the null SHA,
// which is update-index --index-info's removal syntax.
func applyTransitions(ctx context.Context, repoTop, idxPath, base string, trs []Transition) (string, error) {
	if _, err := gitWithIndex(ctx, repoTop, idxPath, nil, "read-tree", base+"^{tree}"); err != nil {
		return "", err
	}
	var stdin bytes.Buffer
	for _, tr := range trs {
		if tr.Result == nil {
			fmt.Fprintf(&stdin, "0 %s\t%s\n", strings.Repeat("0", 40), tr.Path)
			continue
		}
		fmt.Fprintf(&stdin, "%s %s\t%s\n", tr.Result.Mode, tr.Result.SHA, tr.Path)
	}
	if _, err := gitWithIndex(ctx, repoTop, idxPath, &stdin, "update-index", "--index-info"); err != nil {
		return "", err
	}
	return gitWithIndex(ctx, repoTop, idxPath, nil, "write-tree")
}

// commitTree writes the chain commit for one member, carrying its attribution
// — and, on the tip, the batch claim.
func commitTree(ctx context.Context, p PromoteParams, tree, parent string, m member, b *batch, tip bool) (string, error) {
	var msg bytes.Buffer
	fmt.Fprintf(&msg, "loto: promote %s\n\n", m.candidateID)
	fmt.Fprintf(&msg, "Candidate: %s\n", m.candidateID)
	fmt.Fprintf(&msg, "Proposal: %s\n", m.proposalSHA)
	fmt.Fprintf(&msg, "Agent: %s\n", m.env.AgentUUID)
	fmt.Fprintf(&msg, "Session: %s\n", m.env.SessionUUID)
	fmt.Fprintf(&msg, "Bead: %s\n", m.env.BeadID)
	if tip {
		// The claim IS this commit. One object, so the pusher's liveness data
		// and the chain it is holding cannot drift apart.
		fmt.Fprintf(&msg, "%s: %s\n", trailerBatch, b.id)
		fmt.Fprintf(&msg, "%s: %s\n", trailerCandidates, strings.Join(b.candidateIDs(), ","))
		fmt.Fprintf(&msg, "%s: %s\n", trailerHost, p.Host)
		fmt.Fprintf(&msg, "%s: %d\n", trailerPID, p.PID)
		fmt.Fprintf(&msg, "%s: %d\n", trailerProcStart, p.ProcStart)
	}
	cmd := exec.CommandContext(ctx, "git", "commit-tree", tree, "-p", parent)
	cmd.Dir = p.RepoTop
	cmd.Stdin = &msg
	// append, not the reverse: os.Environ() first so the loto-gate identity,
	// appended last, wins on a duplicate key (Go's exec — and the OS beneath
	// it — resolve a duplicated env key to its LAST occurrence). Putting the
	// identity first, as this used to, let any GIT_AUTHOR_NAME/COMMITTER_NAME
	// the caller's shell exported override the very identity this function
	// exists to enforce. Matches gitWithIndex below and internal/lane/verify.go.
	cmd.Env = append(os.Environ(), promoteIdentityEnv()...)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gate: commit-tree: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (b *batch) candidateIDs() []string {
	ids := make([]string, 0, len(b.members))
	for i := range b.members {
		ids = append(ids, b.members[i].candidateID)
	}
	return ids
}

// promoteIdentityEnv fixes the author and committer of chain commits. The
// promoter is the gate, not whichever agent's shell happened to run the push;
// a repo-config user.name would otherwise leak into integration history.
func promoteIdentityEnv() []string {
	return []string{
		"GIT_AUTHOR_NAME=loto-gate",
		"GIT_AUTHOR_EMAIL=loto@localhost",
		"GIT_COMMITTER_NAME=loto-gate",
		"GIT_COMMITTER_EMAIL=loto@localhost",
	}
}

// gitWithIndex runs a git plumbing command against a scratch index file.
//
// ‡ GIT_INDEX_FILE is set per-exec through cmd.Env, never os.Setenv: the
// process-wide variable would be visible to every concurrent git call in this
// process, including a peer goroutine's.
func gitWithIndex(ctx context.Context, repoTop, idxPath string, stdin *bytes.Buffer, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gatePlumbingTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoTop
	cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idxPath)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("%w: git %s", ErrGitTimeout, strings.Join(args, " "))
		}
		return "", fmt.Errorf("gate: git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// promotePhase3 commits the batch: integration advances to the chain tip and
// every ref the batch consumed is retired, all in one transaction.
func promotePhase3(ctx context.Context, p PromoteParams, b *batch) error {
	fl, err := acquirePromoteFlock(ctx, p.RepoTop)
	if err != nil {
		return err
	}
	defer fl.release()

	updates := []RefUpdate{
		{Verb: VerbUpdate, Ref: IntegrationRef, NewSHA: b.chainTip, OldSHA: b.snapshot},
	}
	for i := range b.members {
		m := &b.members[i]
		updates = append(updates,
			RefUpdate{Verb: VerbDelete, Ref: candidateRef(m.candidateID), OldSHA: m.envelopeSHA},
			RefUpdate{Verb: VerbDelete, Ref: proposalRef(m.candidateID), OldSHA: m.proposalSHA},
		)
	}
	updates = append(updates, RefUpdate{Verb: VerbDelete, Ref: promotingRef(b.id), OldSHA: b.chainTip})

	if err := UpdateRefsTx(ctx, p.RepoTop, updates); err != nil {
		if errors.Is(err, ErrRefCASLost) {
			// A genuine lost CAS. Any line failing fails all of them, so there
			// is nothing to undo — only the claim to drop, which the caller
			// does on this branch.
			return fmt.Errorf("%w: %w", errPromoteRace, err)
		}
		// Anything else — an exec failure, ENOSPC, a cancelled context, a
		// gatePlumbingTimeout kill — is infrastructure trouble, not
		// contention. Returning it un-wrapped keeps promoteVerifyAndCommit's
		// and promoteSolo's errors.Is(err, errPromoteRace) branch reserved
		// for real races; everything else falls to their default case, which
		// stops the round instead of retrying a failure retrying cannot fix.
		return fmt.Errorf("gate: promotePhase3: update refs: %w", err)
	}
	return nil
}

// newBatchID mirrors NewCandidateID's recipe, prefixed "b-" so a batch reads
// unambiguously beside a candidate id.
func newBatchID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return "b-" + hex.EncodeToString(b[:])
}

// --- promotion flock ---------------------------------------------------

const (
	promoteFlockPollInitial = 25 * time.Millisecond
	promoteFlockPollMax     = 250 * time.Millisecond
	promoteFlockLimit       = 30 * time.Second
)

// errPromoteFlockTimeout is returned when the promotion flock cannot be taken.
var errPromoteFlockTimeout = errors.New("gate: promotion flock acquire timed out")

type promoteFlock struct{ f *os.File }

func (h *promoteFlock) release() {
	if h == nil || h.f == nil {
		return
	}
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
	h.f = nil
}

// acquirePromoteFlock serializes REF CONSTRUCTION across pusher processes.
//
// ‡ Its own file, not the store's op-flock: that one guards the SQLite
// database and is a different resource with a different hold pattern. It lives
// beside the refs it protects, under --git-common-dir, so two linked worktrees
// of one repo — which SHARE refs — contend on one file rather than flocking
// two different paths while racing on the same namespace.
//
// The kernel releases a flock when the holder dies, so a crashed pusher never
// wedges phase 1; its leftover promoting ref is the durable claim, and
// reclaiming that is a liveness question, not a lock question.
func acquirePromoteFlock(ctx context.Context, repoTop string) (*promoteFlock, error) {
	g := gitRunner{repoTop: repoTop}
	common, err := g.run(ctx, "rev-parse", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("gate: resolve git-common-dir: %w", err)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(repoTop, common)
	}
	path := filepath.Join(common, "loto-promote.flock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("gate: open promotion flock: %w", err)
	}
	deadline := time.Now().Add(promoteFlockLimit)
	backoff := promoteFlockPollInitial
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &promoteFlock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("gate: flock %s: %w", path, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, errPromoteFlockTimeout
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < promoteFlockPollMax {
			backoff *= 2
		}
	}
}
