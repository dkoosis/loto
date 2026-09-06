// Package gate holds the git-gate machinery (loto-ovno): the candidate
// envelope that binds a proposed change to the state it was built against, and
// — in later beads — the admission function and promotion pipeline that check
// it. This package is deliberately independent of internal/store: an envelope
// is a pure function of git object state plus caller-supplied identity and
// lease data, never a store lookup. That keeps Capture testable without a DB
// and keeps this package importable from a future gate binary that doesn't
// want loto's full coordination surface.
//
// Design source: Project/loto/plans/git-gate.md, "Correctness model" (rev 3,
// ratified — decision e4b225b66813 amended 2026-08-17).
package gate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
)

// gatePlumbingTimeout bounds a fast, read-only git call — gitRunner.run's
// ls-tree/rev-parse/for-each-ref/log-1 calls — where a wedge means something
// is actually stuck, not merely working through a big tree. Mirrors
// internal/cli's gitTimeout value; kept as this package's own constant since
// the two latency budgets are free to diverge later.
const gatePlumbingTimeout = 5 * time.Second

// gateTreeTimeout bounds a git call that walks or writes tree/index state —
// gitWithIndex's read-tree/write-tree/update-index, and UpdateRefsTx's
// update-ref --stdin — where a cold packfile or a large tree can legitimately
// take far longer than a plumbing read.
const gateTreeTimeout = 60 * time.Second

// gateWaitDelay bounds how long Cmd.Wait blocks copying a killed git
// subprocess's remaining stdout/stderr after its context expires — cmd.Stdout/
// Stderr here are bytes.Buffers, not *os.File, so exec copies through a pipe
// in a background goroutine that Wait() would otherwise wait on
// indefinitely. Without it, a subprocess that ignores its kill signal (or a
// child it spawned inheriting the pipe) could reintroduce the exact hang
// gatePlumbingTimeout/gateTreeTimeout exist to close off.
const gateWaitDelay = 5 * time.Second

// ErrGitTimeout marks a git subprocess killed by a gate timeout (or an even
// tighter caller-supplied deadline) rather than one that ran to completion
// and failed on its own — the distinction a doctor/operator surface needs to
// tell "git hung" from "git rejected the operation".
var ErrGitTimeout = errors.New("gate: git subprocess timed out")

// gateGitEnv pins LC_ALL=C on every git subprocess this package launches, on
// top of the caller's own environment. refCASRejectionMarker (and any future
// stderr text match) depends on git's untranslated EN-US wording; a LANG/
// LC_ALL the caller's shell happens to export can otherwise translate that
// wording out from under the match, turning a real CAS rejection into a
// misreported infra error (loto-in5f). extra is appended after, so a caller's
// own env override (e.g. GIT_INDEX_FILE) still wins on a duplicate key.
func gateGitEnv(extra ...string) []string {
	return append(append(os.Environ(), "LC_ALL=C"), extra...)
}

// BlobRef pins one path's content identity: a git blob SHA and its file mode
// ("100644" regular, "100755" executable, "120000" symlink). A nil *BlobRef in
// a Transition means the path is absent — no entry at that path in the tree.
type BlobRef struct {
	SHA  string
	Mode string
}

// Transition is the promotion theorem's per-path CAS record (git-gate.md
// "Path-transition CAS"): this path goes from Expected to Result. Expected is
// read from the integration ref, never the working tree — the tree is
// provisional and reintroduces the freshness dependence rev 2 removed. Result
// is read from the proposal commit. Either may be nil: Expected nil means the
// path did not exist in integration at capture time (a creation); Result nil
// means the proposal deletes it.
type Transition struct {
	Path     string
	Expected *BlobRef
	Result   *BlobRef
}

// AncestryEntry is a structural precondition for one CREATED path: the named
// directory must resolve to a TREE in integration — not absent, not a blob —
// both at capture time and, re-checked, at promotion time. This is what closes
// the "pkg/ moved under you" hole: promotion re-checks exactly these entries,
// never a whole parent-tree OID (which would false-conflict on an unrelated
// sibling change). Only created paths (Transition.Expected == nil) carry
// ancestry entries — an existing path's parent directories are trees by
// construction, or the existing blob could not be there.
type AncestryEntry struct {
	// Path is a directory path, e.g. "a" or "a/b" for a created "a/b/c.go".
	Path string
}

// Envelope is the immutable, git-object-bound record admission trusts
// (git-gate.md "Correctness model"). Admission never trusts env vars or
// mutable sidecar files; a candidate ref without its matching envelope is
// rejected outright — that check belongs to the admission function (a later
// bead), not to this type.
type Envelope struct {
	// CandidateID is the caller-assigned identity for this proposal (a later
	// bead mints it and owns the refs/loto/candidates/<id> namespace).
	CandidateID string
	// ProposalSHA is the lane commit this envelope was captured from
	// (lane.Commit's return value).
	ProposalSHA string
	// Base is the commit-ish the proposal was built against — the same value
	// passed as lane.Opts.Base. Recorded for provenance; promotion's freshness
	// check is the per-path Transition/Ancestry data, not this field.
	Base string
	// AgentUUID/SessionUUID identify who proposed this, matching the
	// domain.LockRecord convention so a rejection can be attributed.
	AgentUUID   domain.AgentUUID
	SessionUUID domain.SessionUUID
	// BeadID is the intent/task identity (e.g. "loto-ovno.3") this candidate
	// claims to serve. Free text, not validated here — the beads store is the
	// source of truth for whether it exists.
	BeadID string
	// WriteSet is the exact set of repo-relative paths this candidate touches,
	// sorted. Admission checks the actual diff equals this set (git-gate.md);
	// this type only records what the caller declared.
	WriteSet []string
	// Transitions is one entry per WriteSet path, sorted by Path.
	Transitions []Transition
	// Ancestry is one entry per distinct ancestor directory a created path
	// depends on, sorted by Path, deduplicated across WriteSet.
	Ancestry []AncestryEntry
	// LeaseEpoch is the ownership epoch each WriteSet path was held under at
	// capture time, keyed by path. Supplied by the caller — this package has
	// no store dependency, so it does not look epochs up itself. Admission
	// (a later bead, once the store's epoch column exists) checks these
	// against current lease state; Capture only threads them through.
	LeaseEpoch map[string]int64
}

// CreatedPath is one path this candidate creates, bound to the exact bytes it
// proposed for it. Blob is the proposal commit's blob SHA — Transition.Result
// — and is empty only when the candidate declared a path it neither found nor
// wrote, which attributes nothing.
//
// ‡ The pair, never the path alone (loto-ovno.13 review). A path names a slot;
// a deleter needs to know it is removing the bytes the rejected candidate put
// there, not whatever a peer has since written into the same slot.
type CreatedPath struct {
	Path string
	Blob string
}

// CreatedPaths is the subset of WriteSet this candidate CREATES — every
// transition whose Expected preimage is nil, i.e. the path had no entry in
// integration at capture time — each with the blob the proposal gives it.
// Sorted, because Transitions is.
//
// ‡ This is the only write-set subset a rejection may hand a deleter. A
// MODIFIED path existed before the candidate touched it, so its worktree bytes
// are integration's file with someone's edit on top and deleting it destroys
// content the candidate never authored; a created path's residue is a file
// integration has no entry for at all.
func (e Envelope) CreatedPaths() []CreatedPath {
	var out []CreatedPath
	for _, t := range e.Transitions {
		if t.Expected != nil {
			continue
		}
		cp := CreatedPath{Path: t.Path}
		if t.Result != nil {
			cp.Blob = t.Result.SHA
		}
		out = append(out, cp)
	}
	return out
}

var (
	// errEmptyField is wrapped by Capture's validation for any required,
	// caller-supplied field left blank — a caller bug, not a runtime
	// condition, so a single sentinel covers all of them via errors.Is.
	errEmptyField = errors.New("gate: required field is empty")
	// ErrPathNotBlob is returned when a WriteSet path resolves to something
	// other than a blob or absence in either tree — a directory or a
	// submodule gitlink where a file was expected. lane.Commit's own
	// WriteSet validation (rejectTrackedDirWriteSet) should make this
	// unreachable in practice; Capture still checks, because an envelope
	// with a silently-wrong Transition is worse than a loud rejection.
	ErrPathNotBlob = errors.New("gate: write-set path does not resolve to a file")
	// ErrAncestryNotTree is returned when a created path's ancestor directory
	// does not currently resolve to a tree in the integration ref — the
	// proposal is already stale against integration (someone deleted,
	// renamed, or file-shadowed the parent), and there is nothing later to
	// re-check against. Fail at the cheap end, at capture, rather than
	// minting an envelope that records a false precondition.
	ErrAncestryNotTree = errors.New("gate: ancestor directory does not resolve to a tree in integration")
	// ErrMalformedEnvelope is returned when an envelope's BYTES parse but its
	// contents could not have come from Capture — no write-set, no candidate
	// id, a transition count that does not match the write-set. Exported
	// because admission and promotion both have to tell "this candidate is
	// malformed" apart from "git failed", and only the first is a verdict
	// about the candidate (loto-amkj).
	ErrMalformedEnvelope = errors.New("gate: envelope is malformed")
	// errMalformedLsTreeLine is an internal git-plumbing-parse failure, never a
	// caller-facing condition — wrapped with the offending line for debugging.
	errMalformedLsTreeLine = errors.New("gate: malformed ls-tree line")
)

// CaptureParams is Capture's input. RepoTop, IntegrationRef, ProposalSHA,
// CandidateID, and BeadID are required; WriteSet must be non-empty.
type CaptureParams struct {
	RepoTop        string
	IntegrationRef string // commit-ish; expected preimages are read here
	ProposalSHA    string
	Base           string
	WriteSet       []string
	CandidateID    string
	Agent          domain.AgentUUID
	Session        domain.SessionUUID
	BeadID         string
	// LeaseEpoch is optional per-path epoch data. A path absent from the map
	// carries epoch 0 in the envelope — admission (once it exists) is where a
	// missing epoch becomes a rejection, not here.
	LeaseEpoch map[string]int64
}

func (p CaptureParams) validate() error {
	for name, v := range map[string]string{
		"RepoTop": p.RepoTop, "IntegrationRef": p.IntegrationRef,
		"ProposalSHA": p.ProposalSHA, "CandidateID": p.CandidateID, "BeadID": p.BeadID,
	} {
		if v == "" {
			return fmt.Errorf("%w: %s", errEmptyField, name)
		}
	}
	if len(p.WriteSet) == 0 {
		return fmt.Errorf("%w: WriteSet", errEmptyField)
	}
	return nil
}

// Capture builds an Envelope by reading git object state: Expected from
// IntegrationRef, Result from ProposalSHA, and ancestry entries for every
// created path's ancestor directories. It runs no git WRITE — pure read over
// two tree-ish arguments — so it is safe to call speculatively before any
// admission decision.
func Capture(ctx context.Context, p CaptureParams) (Envelope, error) {
	if err := p.validate(); err != nil {
		return Envelope{}, err
	}
	writeSet := append([]string(nil), p.WriteSet...)
	sort.Strings(writeSet)
	g := gitRunner{repoTop: p.RepoTop}

	transitions, createdPaths, err := captureTransitions(ctx, g, p, writeSet)
	if err != nil {
		return Envelope{}, err
	}
	ancestry, err := captureAncestry(ctx, g, p.IntegrationRef, createdPaths)
	if err != nil {
		return Envelope{}, err
	}

	epoch := make(map[string]int64, len(writeSet))
	for _, wp := range writeSet {
		epoch[wp] = p.LeaseEpoch[wp] // zero value when absent, deliberately
	}

	return Envelope{
		CandidateID: p.CandidateID,
		ProposalSHA: p.ProposalSHA,
		Base:        p.Base,
		AgentUUID:   p.Agent,
		SessionUUID: p.Session,
		BeadID:      p.BeadID,
		WriteSet:    writeSet,
		Transitions: transitions,
		Ancestry:    ancestry,
		LeaseEpoch:  epoch,
	}, nil
}

// captureTransitions reads Expected (from integration) and Result (from the
// proposal) for every write-set path, in order, and returns the paths whose
// Expected is nil — the creations ancestry-checking needs next.
func captureTransitions(ctx context.Context, g gitRunner, p CaptureParams, writeSet []string) ([]Transition, []string, error) {
	transitions := make([]Transition, 0, len(writeSet))
	var created []string
	for _, wp := range writeSet {
		expected, err := g.blobAt(ctx, p.IntegrationRef, wp)
		if err != nil {
			return nil, nil, err
		}
		result, err := g.blobAt(ctx, p.ProposalSHA, wp)
		if err != nil {
			return nil, nil, err
		}
		transitions = append(transitions, Transition{Path: wp, Expected: expected, Result: result})
		if expected == nil {
			created = append(created, wp)
		}
	}
	return transitions, created, nil
}

// captureAncestry computes the deduplicated ancestor-directory set for every
// created path and verifies each one now, not just records it: "records that
// a/ resolves to a tree" presupposes that is currently true. Promotion's job
// is to RE-check the same fact later; capture's job is to check it once, so a
// stale proposal is rejected here rather than minting an envelope that
// already contradicts integration (git-gate.md "fail at the cheap end").
func captureAncestry(ctx context.Context, g gitRunner, integrationRef string, createdPaths []string) ([]AncestryEntry, error) {
	ancestrySet := map[string]bool{}
	for _, wp := range createdPaths {
		for _, anc := range ancestorsOf(wp) {
			ancestrySet[anc] = true
		}
	}
	ancestry := make([]AncestryEntry, 0, len(ancestrySet))
	for a := range ancestrySet {
		isTree, err := g.treeAt(ctx, integrationRef, a)
		if err != nil {
			return nil, err
		}
		if !isTree {
			return nil, fmt.Errorf("%w: %s", ErrAncestryNotTree, a)
		}
		ancestry = append(ancestry, AncestryEntry{Path: a})
	}
	sort.Slice(ancestry, func(i, j int) bool { return ancestry[i].Path < ancestry[j].Path })
	return ancestry, nil
}

// ancestorsOf returns path's ancestor directories, nearest first, stopping
// before the repo root (path.Dir on a top-level file returns "."; "." is not
// a real tree entry anywhere and every promotion already targets the tree
// root, so it carries no information and is excluded).
func ancestorsOf(p string) []string {
	var out []string
	for dir := path.Dir(p); dir != "." && dir != "/"; dir = path.Dir(dir) {
		out = append(out, dir)
	}
	return out
}

// gitRunner is Capture's minimal git plumbing surface — just enough to read
// tree state, no working-tree or index operations. Kept separate from
// internal/lane's gitRunner: that one seeds and mutates a per-lane index,
// which is machinery this read-only package has no reason to depend on.
type gitRunner struct{ repoTop string }

// blobAt resolves path inside tree-ish, returning nil (not an error) when the
// path is absent — the git-gate correctness model treats absence as an
// ordinary Transition state, not a failure. `git ls-tree <tree-ish> -- <path>`
// resolves a nested path directly without -r (verified empirically: it walks
// into subtrees for an exact pathspec) and prints nothing, exit 0, when
// nothing matches — so emptiness IS the absent case, not an error path.
func (g gitRunner) blobAt(ctx context.Context, treeish, path string) (*BlobRef, error) {
	out, err := g.run(ctx, "ls-tree", treeish, "--", path)
	if err != nil {
		return nil, fmt.Errorf("gate: ls-tree %s %s: %w", treeish, path, err)
	}
	if out == "" {
		return nil, nil //nolint:nilnil // (nil, nil) signals "path absent"; a normal Transition state, not an error
	}
	mode, kind, sha, err := parseLsTreeLine(out)
	if err != nil {
		return nil, fmt.Errorf("gate: parse ls-tree %s %s: %w", treeish, path, err)
	}
	if kind != "blob" {
		return nil, fmt.Errorf("%w: %s at %s is a %s", ErrPathNotBlob, path, treeish, kind)
	}
	return &BlobRef{SHA: sha, Mode: mode}, nil
}

// treeAt reports whether path resolves to a tree (directory) inside treeish —
// the ancestry precondition's read. Absence and "resolves to a blob" both
// answer false; the caller only needs the single yes/no this check makes.
func (g gitRunner) treeAt(ctx context.Context, treeish, path string) (bool, error) {
	out, err := g.run(ctx, "ls-tree", treeish, "--", path)
	if err != nil {
		return false, fmt.Errorf("gate: ls-tree %s %s: %w", treeish, path, err)
	}
	if out == "" {
		return false, nil
	}
	_, kind, _, err := parseLsTreeLine(out)
	if err != nil {
		return false, fmt.Errorf("gate: parse ls-tree %s %s: %w", treeish, path, err)
	}
	return kind == "tree", nil
}

// run executes one read-only git plumbing call against repoTop and returns
// trimmed stdout. No stdin, no working-tree touch — every caller in this file
// only ever needs a tree-ish and a pathspec.
func (g gitRunner) run(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gatePlumbingTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.repoTop
	cmd.Env = gateGitEnv()
	cmd.WaitDelay = gateWaitDelay
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("%w: git %s", ErrGitTimeout, strings.Join(args, " "))
		}
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

// parseLsTreeLine splits one `git ls-tree` output line: "<mode> <type> <sha>\t<path>".
// Only the first line is consumed — a single exact pathspec against a single
// path never yields more than one entry.
func parseLsTreeLine(out string) (mode, kind, sha string, err error) {
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(strings.SplitN(line, "\t", 2)[0])
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("%w: %q", errMalformedLsTreeLine, line)
	}
	return fields[0], fields[1], fields[2], nil
}
