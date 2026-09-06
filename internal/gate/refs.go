package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"syscall"
)

// RefUpdate is one line of a git-update-ref transaction (git-update-ref(1),
// "Transactional updates" / the --stdin protocol). Verb selects the git
// subcommand-shaped line: "create" (ref must not already exist — the natural
// double-admission guard for a fresh candidate id), "update" (unconditional or
// old-value-checked), or "delete".
// Verb values RefUpdate accepts — the three git-update-ref --stdin commands
// this helper speaks.
const (
	VerbCreate = "create"
	VerbUpdate = "update"
	VerbDelete = "delete"
)

type RefUpdate struct {
	Verb   string // VerbCreate, VerbUpdate, or VerbDelete
	Ref    string // e.g. "refs/loto/candidates/<id>"
	NewSHA string // required for create/update; ignored for delete
	OldSHA string // optional compare-and-swap value for update/delete; "" = unconditional
}

// ErrEmptyTransaction guards against calling UpdateRefsTx with nothing to do —
// an accidental empty transaction would otherwise silently no-op rather than
// signal the caller built its update list wrong.
var ErrEmptyTransaction = errors.New("gate: empty ref transaction")

// errUnknownRefVerb is wrapped with the offending verb/ref via %w — a caller
// bug (a typo'd Verb field), not a runtime condition worth its own exported
// sentinel.
var errUnknownRefVerb = errors.New("gate: unknown ref update verb")

// ErrRefCASLost marks the one UpdateRefsTx failure shape that actually means
// "lost the compare-and-swap": git-update-ref(1) rejected an update/delete
// line because the ref's current value no longer matches the OldSHA supplied.
// Every other UpdateRefsTx failure (exec error, ENOSPC, a cancelled context,
// ErrGitTimeout) is infrastructure trouble, not contention, and must not be
// mistaken for one by a caller doing errors.Is(err, ErrRefCASLost).
var ErrRefCASLost = errors.New("gate: ref update rejected — old value no longer matches (lost the compare-and-swap)")

// refCASRejectionMarker is the exact substring git-update-ref --stdin prints
// on stderr for a lost CAS: "cannot lock ref '<ref>': is at <old> but
// expected <want>" (confirmed empirically against the git on PATH,
// 2026-09-06). No other update-ref --stdin failure shape uses this wording —
// a missing object says "nonexistent object", a bad verb never reaches git at
// all — so a substring match is unambiguous.
const refCASRejectionMarker = "but expected"

// UpdateRefsTx runs one atomic multi-ref transaction via
// `git update-ref --stdin`: every listed update applies, or none does
// (git-gate.md "one atomic `update-ref --stdin` transaction... after a crash,
// refs alone reconstruct state"). This is what lets admission write the
// candidate ref and the proposal ref together — a crash between the two would
// otherwise leave a candidate ref pointing at an envelope whose proposal
// commit nothing keeps reachable, or vice versa.
func UpdateRefsTx(ctx context.Context, repoTop string, updates []RefUpdate) error {
	if len(updates) == 0 {
		return ErrEmptyTransaction
	}
	stdin, err := buildRefUpdateStdin(updates)
	if err != nil {
		return err
	}

	// gateTreeTimeout, not gatePlumbingTimeout: acquiring every ref lock in
	// one transaction under contention can take longer than a single
	// plumbing read.
	ctx, cancel := context.WithTimeout(ctx, gateTreeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "update-ref", "--stdin")
	cmd.Dir = repoTop
	cmd.Stdin = stdin
	cmd.Env = gateGitEnv()
	cmd.WaitDelay = gateWaitDelay
	// On timeout, SIGTERM first so git gets a chance to remove the
	// <ref>.lock files it is holding, instead of the default CommandContext
	// behavior (an immediate SIGKILL that leaves them in place).
	//
	// ‡ This narrows, but does not close, the stale-lock window: git must
	// still receive and act on SIGTERM before its own shutdown path finishes
	// removing every lock it holds. WaitDelay's eventual SIGKILL — or a
	// signal git does not catch in time — can still leave a lock file behind
	// for doctor to find and clear.
	cmd.Cancel = func() error {
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("%w: git update-ref --stdin", ErrGitTimeout)
		}
		msg := strings.TrimSpace(stderr.String())
		if strings.Contains(msg, refCASRejectionMarker) {
			return fmt.Errorf("%w: %s", ErrRefCASLost, msg)
		}
		return fmt.Errorf("gate: update-ref --stdin: %w: %s", err, msg)
	}
	return nil
}

// buildRefUpdateStdin renders updates into the git-update-ref --stdin
// transaction script: a "start" line, one create/update/delete line per
// update, and a "commit" line. Split out of UpdateRefsTx so the per-verb
// branching doesn't count against that function's own cognitive complexity.
func buildRefUpdateStdin(updates []RefUpdate) (*bytes.Buffer, error) {
	var stdin bytes.Buffer
	stdin.WriteString("start\n")
	for _, u := range updates {
		if err := writeRefUpdateLine(&stdin, u); err != nil {
			return nil, err
		}
	}
	stdin.WriteString("commit\n")
	return &stdin, nil
}

// writeRefUpdateLine appends one RefUpdate's update-ref --stdin command line.
func writeRefUpdateLine(w *bytes.Buffer, u RefUpdate) error {
	switch u.Verb {
	case VerbCreate:
		fmt.Fprintf(w, "create %s %s\n", u.Ref, u.NewSHA)
	case VerbUpdate:
		if u.OldSHA != "" {
			fmt.Fprintf(w, "update %s %s %s\n", u.Ref, u.NewSHA, u.OldSHA)
		} else {
			fmt.Fprintf(w, "update %s %s\n", u.Ref, u.NewSHA)
		}
	case VerbDelete:
		if u.OldSHA != "" {
			fmt.Fprintf(w, "delete %s %s\n", u.Ref, u.OldSHA)
		} else {
			fmt.Fprintf(w, "delete %s\n", u.Ref)
		}
	default:
		return fmt.Errorf("%w: %q for %s", errUnknownRefVerb, u.Verb, u.Ref)
	}
	return nil
}

// WriteCandidateRefs is the accepted-candidate write: candidateRef(id) → the
// envelope blob, proposalRef(id) → the proposal commit, in ONE atomic
// transaction. Both use "create" — a fresh candidate id must not already have
// refs; a collision here means a caller reused an id, and failing loudly beats
// silently overwriting whichever candidate got there first.
func WriteCandidateRefs(ctx context.Context, repoTop, candidateID, envelopeSHA, proposalSHA string) error {
	return UpdateRefsTx(ctx, repoTop, []RefUpdate{
		{Verb: VerbCreate, Ref: candidateRef(candidateID), NewSHA: envelopeSHA},
		{Verb: VerbCreate, Ref: proposalRef(candidateID), NewSHA: proposalSHA},
	})
}

// candidateRef and proposalRef are the two ref trees an accepted candidate
// occupies, kept structurally distinct (never nested under one another —
// git's loose-ref storage can't have both a ref file AT a path and a ref file
// inside a same-named directory). candidateRef points at the envelope BLOB —
// so "a candidate ref without its envelope" (git-gate.md) cannot even arise
// structurally, the ref IS the envelope pointer. proposalRef points at the
// PROPOSAL COMMIT, which candidateRef's blob does not itself keep reachable
// (a blob's content is opaque bytes to git, not an object reference) — without
// this second ref, the commit becomes GC-eligible the moment its lane branch
// moves past it in a later wave.
// candidateRefPrefix is the one spelling of the candidates namespace — the
// ref builder and the ref lister must not drift apart, or a residue scan
// silently matches nothing.
const candidateRefPrefix = "refs/loto/candidates/"

func candidateRef(id string) string { return candidateRefPrefix + id }
func proposalRef(id string) string  { return "refs/loto/proposals/" + id }

// ListCandidateIDs returns the candidate id of every refs/loto/candidates/*
// ref in the repo, sorted, so a caller can tell an accepted candidate from
// acceptance residue.
//
// ‡ An error is never an empty list. AcceptCandidate writes the claims first
// and the refs last, so "this candidate has no ref" is exactly the signal that
// its acceptance never completed — which makes a failed ref read indisputably
// dangerous to treat as "no refs exist": every live candidate's claims would
// read as residue and a repair pass would delete them. Callers must abort on
// the error rather than fall back to the zero value.
func ListCandidateIDs(ctx context.Context, repoTop string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname)", candidateRefPrefix)
	cmd.Dir = repoTop
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gate: for-each-ref %s: %w: %s", candidateRefPrefix, err, strings.TrimSpace(stderr.String()))
	}
	var ids []string
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		if line == "" {
			continue
		}
		ids = append(ids, strings.TrimPrefix(line, candidateRefPrefix))
	}
	sort.Strings(ids)
	return ids, nil
}

// promotingRefPrefix is the namespace a pusher claims a batch under while it
// verifies. ONE ref per batch, pointing at the prospective chain's TIP COMMIT:
// that single object is both the reachability anchor (phase 2 runs with no
// flock held, so nothing else keeps the chain alive) and the claim itself —
// the promoter's host/pid/proc-start ride as trailers in its message. A
// separate claim blob would be a second object that can diverge from the
// commit it describes; one object cannot disagree with itself.
const promotingRefPrefix = "refs/loto/promoting/"

func promotingRef(batch string) string { return promotingRefPrefix + batch }

// listRefsWithSHAs returns refname → object SHA for every ref under prefix,
// with the prefix stripped from the key.
//
// ‡ Same abort-on-error contract as ListCandidateIDs, for the same reason: a
// failed ref read must never read as "nothing is there". Promotion uses this
// to find live promoting claims (an empty map would mean stealing a peer's
// in-flight batch) and to read the OldSHA values its CAS deletes depend on (an
// empty map would mean deleting refs unconditionally).
func listRefsWithSHAs(ctx context.Context, repoTop, prefix string) (map[string]string, error) {
	cmd := exec.CommandContext(ctx, "git", "for-each-ref", "--format=%(refname) %(objectname)", prefix)
	cmd.Dir = repoTop
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("gate: for-each-ref %s: %w: %s", prefix, err, strings.TrimSpace(stderr.String()))
	}
	out := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(stdout.String()), "\n") {
		name, sha, ok := strings.Cut(line, " ")
		if !ok || name == "" {
			continue
		}
		out[strings.TrimPrefix(name, prefix)] = sha
	}
	return out, nil
}
