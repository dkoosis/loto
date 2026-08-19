package gate

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// RejectionReason names why Admit refused a candidate — the subset of
// git-gate.md's rejection taxonomy this bead's checks can actually produce.
// The remaining classes (violation-intersect's REAL logic, verify-red,
// verify-infrastructure, promotion-race) belong to later beads (.7, .9); this
// bead's violation-intersect check is a deliberate no-op until the violation
// store ships (loto-ovno.9) — see checkViolationIntersect.
type RejectionReason string

const (
	ReasonProposalSHAMismatch RejectionReason = "proposal-sha-mismatch"
	// ReasonUnauthorizedPath: the proposal's actual diff touches a path outside
	// the declared write-set, OR the write-set declares a path the diff never
	// touches — either way the envelope's WriteSet no longer describes the
	// commit it claims to (git-gate.md rejection taxonomy: "unauthorized-path").
	ReasonUnauthorizedPath   RejectionReason = "unauthorized-path"
	ReasonStaleLeaseEpoch    RejectionReason = "stale-lease-epoch"
	ReasonStalePreimage      RejectionReason = "stale-preimage"
	ReasonStaleAncestry      RejectionReason = "stale-ancestry"
	ReasonMalformedCandidate RejectionReason = "malformed-candidate"
)

// Decision is Admit's verdict. Reason and Detail are zero-value on Accepted.
type Decision struct {
	Accepted bool
	Reason   RejectionReason
	Detail   string // which path, which epoch mismatch — actionable, not just a class
}

func accept() Decision                                 { return Decision{Accepted: true} }
func reject(r RejectionReason, detail string) Decision { return Decision{Reason: r, Detail: detail} }

// AdmitParams is everything Admit needs beyond the envelope itself, all read
// by the CALLER from live state immediately before calling. Admission
// re-validates against NOW — it never trusts the envelope's own captured
// snapshot for anything except WHAT to check (git-gate.md: "expected
// preimages are read from refs/loto/integration at capture time — never the
// working tree"; Capture's job was recording that snapshot, Admit's job is
// confirming it still holds).
//
// ‡ Deliberately store-independent, matching envelope.go's own invariant one
// level further: CurrentEpoch is a plain map the caller populates from
// whatever store it uses, not a live lookup this package performs. Keeps
// Admit testable with zero DB and zero live git repo beyond the two
// tree-ish/commit-ish values it actually reads.
type AdmitParams struct {
	RepoTop string
	// PresentedProposalSHA is the commit under review — checked against
	// env.ProposalSHA so a caller cannot silently admit a DIFFERENT commit
	// than the one the envelope was captured for.
	PresentedProposalSHA string
	// IntegrationRef is re-read fresh, not the value the envelope captured —
	// this is precisely what makes ReasonStalePreimage/StaleAncestry
	// meaningful: they compare the envelope's OLD snapshot against integration
	// as it stands RIGHT NOW.
	IntegrationRef string
	// CurrentEpoch is path -> the CURRENT locks.epoch value, read by the
	// caller immediately before calling Admit (loto-ovno.2's epoch column). A
	// path absent from the map reads as epoch 0, matching a fresh/never-locked
	// path and legacy rows uniformly.
	CurrentEpoch map[string]int64
}

// Admit decides whether env may be promoted, checking in the order
// git-gate.md's admission bullet lists them — cheap structural checks before
// the git-plumbing re-reads, so a malformed candidate never pays for a tree
// walk it was always going to fail anyway.
func Admit(ctx context.Context, env Envelope, p AdmitParams) (Decision, error) {
	if env.ProposalSHA != p.PresentedProposalSHA {
		return reject(ReasonProposalSHAMismatch, fmt.Sprintf("envelope names %s, presented %s", env.ProposalSHA, p.PresentedProposalSHA)), nil
	}
	if d, err := checkCommitShape(ctx, p.RepoTop, env.ProposalSHA); err != nil {
		return Decision{}, err
	} else if !d.Accepted {
		return d, nil
	}
	if d, err := checkDiffMatchesWriteSet(ctx, p.RepoTop, env); err != nil {
		return Decision{}, err
	} else if !d.Accepted {
		return d, nil
	}
	if d := checkLeaseEpochs(env, p.CurrentEpoch); !d.Accepted {
		return d, nil
	}
	if d, err := checkTransitionsCurrent(ctx, p.RepoTop, env, p.IntegrationRef); err != nil {
		return Decision{}, err
	} else if !d.Accepted {
		return d, nil
	}
	if d, err := checkAncestryCurrent(ctx, p.RepoTop, env, p.IntegrationRef); err != nil {
		return Decision{}, err
	} else if !d.Accepted {
		return d, nil
	}
	if d := checkViolationIntersect(env); !d.Accepted {
		return d, nil
	}
	return accept(), nil
}

// checkCommitShape rejects a proposal commit with anything other than exactly
// one parent. lane.Commit always builds a single-parent commit-tree; more than
// one parent (a merge slipping in some other way) or zero (a root commit,
// which a real Base always precludes) is not a shape this pipeline's
// promotion theorem was built to reason about — reject rather than silently
// promote something the CAS model never modeled.
func checkCommitShape(ctx context.Context, repoTop, commit string) (Decision, error) {
	g := gitRunner{repoTop: repoTop}
	out, err := g.run(ctx, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return Decision{}, fmt.Errorf("gate: rev-list --parents %s: %w", commit, err)
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return reject(ReasonMalformedCandidate, "commit not found: "+commit), nil
	}
	parents := len(fields) - 1 // first field is the commit itself
	if parents != 1 {
		return reject(ReasonMalformedCandidate, fmt.Sprintf("commit %s has %d parent(s), want exactly 1", commit, parents)), nil
	}
	return accept(), nil
}

// checkDiffMatchesWriteSet re-derives the proposal's actual changed-path set
// via diff-tree against its first parent and requires EXACT equality with the
// envelope's declared WriteSet — not a subset either direction. A path in the
// diff but not the write-set means the candidate touched ground it never
// declared (and never held a lease epoch for); a write-set path absent from
// the diff means the envelope over-declares what the commit does, which would
// let admission validate epochs/preimages for paths the promotion itself
// never actually changes.
func checkDiffMatchesWriteSet(ctx context.Context, repoTop string, env Envelope) (Decision, error) {
	g := gitRunner{repoTop: repoTop}
	out, err := g.run(ctx, "diff-tree", "--no-commit-id", "--name-only", "-r", env.ProposalSHA)
	if err != nil {
		return Decision{}, fmt.Errorf("gate: diff-tree %s: %w", env.ProposalSHA, err)
	}
	actual := strings.Split(strings.TrimSpace(out), "\n")
	if len(actual) == 1 && actual[0] == "" {
		actual = []string{}
	}
	sort.Strings(actual)
	declared := append([]string(nil), env.WriteSet...)
	sort.Strings(declared)

	// diffStringSets(declared, actual) returns (onlyDeclared, onlyActual):
	// onlyDeclared = write-set paths the diff never touches; onlyActual = diff
	// paths the write-set never declared (the unauthorized ones).
	onlyDeclared, onlyActual := diffStringSets(declared, actual)
	if len(onlyActual) > 0 {
		return reject(ReasonUnauthorizedPath, "diff touches undeclared path(s): "+strings.Join(onlyActual, ", ")), nil
	}
	if len(onlyDeclared) > 0 {
		return reject(ReasonUnauthorizedPath, "write-set declares path(s) the diff never touches: "+strings.Join(onlyDeclared, ", ")), nil
	}
	return accept(), nil
}

// checkLeaseEpochs requires the envelope's captured epoch to still match the
// path's CURRENT epoch, for every write-set path. A mismatch means the lease
// was released and re-granted (or transferred, or reclaimed) since capture —
// this candidate's authorization no longer describes reality, regardless of
// whether the CONTENT at that path also changed.
func checkLeaseEpochs(env Envelope, current map[string]int64) Decision {
	for _, path := range env.WriteSet {
		want := env.LeaseEpoch[path]
		got := current[path]
		if want != got {
			return reject(ReasonStaleLeaseEpoch, fmt.Sprintf("%s: captured epoch=%d, current epoch=%d", path, want, got))
		}
	}
	return accept()
}

// checkTransitionsCurrent re-reads each transition's Expected side from
// IntegrationRef AS IT STANDS RIGHT NOW and requires it still match what the
// envelope captured — the promotion theorem's CAS check, re-verified at
// admission rather than trusted from capture time. A mismatch means a peer's
// candidate already promoted a conflicting change to this path.
func checkTransitionsCurrent(ctx context.Context, repoTop string, env Envelope, integrationRef string) (Decision, error) {
	g := gitRunner{repoTop: repoTop}
	for _, tr := range env.Transitions {
		now, err := g.blobAt(ctx, integrationRef, tr.Path)
		if err != nil {
			return Decision{}, err
		}
		if !blobRefsEqual(tr.Expected, now) {
			return reject(ReasonStalePreimage, tr.Path+": captured preimage no longer matches integration"), nil
		}
	}
	return accept(), nil
}

// checkAncestryCurrent re-verifies every ancestry entry still resolves as a
// tree in CURRENT integration — the "pkg/ moved under you" re-check.
func checkAncestryCurrent(ctx context.Context, repoTop string, env Envelope, integrationRef string) (Decision, error) {
	g := gitRunner{repoTop: repoTop}
	for _, a := range env.Ancestry {
		isTree, err := g.treeAt(ctx, integrationRef, a.Path)
		if err != nil {
			return Decision{}, err
		}
		if !isTree {
			return reject(ReasonStaleAncestry, a.Path+": no longer resolves to a tree in integration"), nil
		}
	}
	return accept(), nil
}

// checkViolationIntersect is a deliberate no-op: "write-set intersects no
// unresolved violation... activates once the sensor exists" (loto-ovno.4's
// own bead description; the sticky-violation store is loto-ovno.9). Kept as
// its own named check, called from Admit in the position git-gate.md lists
// it, so wiring the real logic in later is a body-only change with no
// call-site edit.
func checkViolationIntersect(_ Envelope) Decision {
	return accept()
}

func blobRefsEqual(a, b *BlobRef) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.SHA == b.SHA && a.Mode == b.Mode
}

// diffStringSets returns (in a, not b) and (in b, not a) for two SORTED
// string slices — a single merge-style pass rather than building two maps for
// what is, in practice, a handful of paths per candidate.
func diffStringSets(a, b []string) (onlyA, onlyB []string) {
	if a == nil {
		a = []string{}
	}
	if b == nil {
		b = []string{}
	}
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		switch {
		case a[i] == b[j]:
			i++
			j++
		case a[i] < b[j]:
			onlyA = append(onlyA, a[i])
			i++
		default:
			onlyB = append(onlyB, b[j])
			j++
		}
	}
	onlyA = append(onlyA, a[i:]...)
	onlyB = append(onlyB, b[j:]...)
	return onlyA, onlyB
}
