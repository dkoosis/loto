package cli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"

	"loto/internal/identity"
	"loto/internal/render"
)

// Presence states one session can put on a worktree, in the vocabulary the
// report prints.
const (
	presenceOccupied = "occupied"
	presenceUnknown  = "unknown"
)

// Presence sources: which record placed the session.
const (
	presenceSourceLoto   = "loto"
	presenceSourceClaude = "claude"
)

// reasonNoLotoRecord is the unknown reason for a live Claude Code session loto
// holds no live record of.
const reasonNoLotoRecord = "no-loto-record"

// lotoSession is one loto session record with its liveness verdict, the
// input decideWorktreePresence takes so the decision stays pure.
type lotoSession struct {
	Rec      identity.SessionRecord
	Liveness identity.SessionLiveness
	Reason   string
}

// claudeSession is one Claude Code session record with its liveness.
type claudeSession struct {
	Rec      identity.ClaudeSession
	Liveness identity.SessionLiveness
}

// presenceVerdict is one other session's standing at the worktree asked about.
type presenceVerdict struct {
	Session string
	PID     int
	State   string // presenceOccupied | presenceUnknown
	Source  string // presenceSourceLoto | presenceSourceClaude
	At      string // the path that placed it, "" when none did
	Reason  string // liveness reason; set on unknown
}

// presenceScope is the worktree asked about and the repo it belongs to, in
// canonical spelling, plus how to compare a recorded path against them.
type presenceScope struct {
	Target string
	Trees  []string // every checkout of the target's repo
	// Fold compares case-insensitively. On APFS a session record and a
	// caller can spell one directory two ways (~/Projects vs ~/projects),
	// and EvalSymlinks keeps whichever spelling it was handed: compared
	// byte-wise, a live session in ~/Projects/x read free from ~/projects/x.
	Fold bool
}

func (sc presenceScope) norm(p string) string {
	if sc.Fold {
		return strings.ToLower(p)
	}
	return p
}

// atOrInside reports whether p is root or a path under it.
func (sc presenceScope) atOrInside(p, root string) bool {
	if p == "" || root == "" {
		return false
	}
	p, root = sc.norm(p), sc.norm(strings.TrimSuffix(root, string(filepath.Separator)))
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

func (sc presenceScope) inTarget(p string) bool { return sc.atOrInside(p, sc.Target) }

// inRepo reports whether p sits at or inside any checkout of the repo.
func (sc presenceScope) inRepo(p string) bool {
	for _, t := range sc.Trees {
		if sc.atOrInside(p, t) {
			return true
		}
	}
	return false
}

// decideWorktreePresence judges every session other than the caller against
// sc's target and repo. Callers are
// already filtered out. canon resolves a recorded path the way target was
// resolved, so a /var vs /private/var spelling cannot hide a session.
//
// The rule, per session:
//
//	dead                                         → nothing
//	live, a recorded checkout at/inside target   → occupied
//	live loto record whose Claude cwd is in target → occupied (the Claude record counts too)
//	loto liveness unknown, checkout in this repo → unknown
//	live Claude session, no live loto record, cwd in this repo → unknown
//
// ‡ Absence of a loto record is never "free". A live Claude Code session loto
// cannot place — whoami never ran, or its record will not verdict — is a
// session that may be sitting in any checkout of its repo, so the answer for
// that repo is unknown and the caller keeps the tree (loto-nwoz).
func decideWorktreePresence(sc presenceScope, lotoRecs []lotoSession, ccRecs []claudeSession, canon func(string) string) []presenceVerdict {
	set := presenceSet{}
	placedSID := map[string]bool{}
	placedPID := map[int]bool{}
	for i := range lotoRecs {
		s := &lotoRecs[i]
		if s.Liveness == identity.SessionLive {
			placedSID[s.Rec.SessionID] = true
			// A pid alone may be recycled: it places a Claude record under
			// another session id (/clear rotates it) only once the start
			// time proved the pid is still this record's process.
			if s.Rec.PID > 0 && identity.PIDProven(s.Reason) {
				placedPID[s.Rec.PID] = true
			}
		}
		if v, ok := judgeLotoSession(s, sc, canon); ok {
			set.put(v)
		}
	}
	for i := range ccRecs {
		s := &ccRecs[i]
		placed := placedSID[s.Rec.SessionID] || placedPID[s.Rec.PID]
		if v, ok := judgeClaudeSession(s, sc, placed, canon); ok {
			set.put(v)
		}
	}
	return set.sorted()
}

// judgeLotoSession places one loto record: occupied when live and a recorded
// checkout is at or inside target, unknown when its liveness cannot be decided
// and a recorded checkout is in this repo.
func judgeLotoSession(s *lotoSession, sc presenceScope, canon func(string) string) (presenceVerdict, bool) {
	v := presenceVerdict{Session: s.Rec.SessionID, PID: s.Rec.PID, Source: presenceSourceLoto}
	switch s.Liveness {
	case identity.SessionLive:
		for _, p := range recordPaths(&s.Rec, canon) {
			if sc.inTarget(p) {
				v.State, v.At = presenceOccupied, p
				return v, true
			}
		}
	case identity.SessionUnknown:
		for _, p := range recordPaths(&s.Rec, canon) {
			if sc.inRepo(p) {
				v.State, v.At, v.Reason = presenceUnknown, p, s.Reason
				return v, true
			}
		}
	case identity.SessionDead:
	}
	return v, false
}

// judgeClaudeSession places one Claude Code record. placed says a live loto
// record already accounts for this session, so its cwd elsewhere in the repo
// is not a reason for unknown.
func judgeClaudeSession(s *claudeSession, sc presenceScope, placed bool, canon func(string) string) (presenceVerdict, bool) {
	if s.Liveness == identity.SessionDead {
		return presenceVerdict{}, false
	}
	key := s.Rec.SessionID
	if key == "" {
		key = "pid:" + strconv.Itoa(s.Rec.PID)
	}
	v := presenceVerdict{Session: key, PID: s.Rec.PID, Source: presenceSourceClaude, At: canon(s.Rec.CWD)}
	switch {
	case sc.inTarget(v.At):
		v.State = presenceOccupied
	case !placed && sc.inRepo(v.At):
		v.State, v.Reason = presenceUnknown, reasonNoLotoRecord
	default:
		return v, false
	}
	return v, true
}

// presenceSet holds one verdict per session; occupied outranks unknown, and
// the first verdict of equal rank stands.
type presenceSet map[string]presenceVerdict

func (ps presenceSet) put(v presenceVerdict) {
	if old, ok := ps[v.Session]; ok && (old.State == presenceOccupied || v.State != presenceOccupied) {
		return
	}
	ps[v.Session] = v
}

// sorted returns the verdicts occupied first, then by session id, so the same
// records always print the same report.
func (ps presenceSet) sorted() []presenceVerdict {
	out := make([]presenceVerdict, 0, len(ps))
	for _, v := range ps {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].State != out[j].State {
			return out[i].State == presenceOccupied
		}
		return out[i].Session < out[j].Session
	})
	return out
}

// recordPaths is every checkout a loto record places its session in: where it
// is now and where it started. Counting the start too errs toward occupied.
func recordPaths(r *identity.SessionRecord, canon func(string) string) []string {
	var out []string
	for _, p := range []string{r.Checkout, r.Repo} {
		if p == "" {
			continue
		}
		if c := canon(p); !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out
}

// canonPath is the one spelling every presence path is compared in: absolute,
// cleaned, symlinks resolved. A path that no longer exists keeps its cleaned
// form — nobody is working inside a directory that is gone.
func canonPath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// runCheckWorktree answers "is any other live session working in this
// worktree?" — the presence query a tool owes before deleting a worktree
// (loto-nwoz). Locks are not the question: a session sitting in a clean,
// pushed tree holds none, and sdlc tidy deleted such a tree as unused.
//
// exit 0 free · 1 occupied · 3 unknown (a session loto cannot place, or the
// path cannot be resolved) · 2 usage. Every non-zero means keep the tree.
func runCheckWorktree(ctx context.Context, path string, stdout, stderr io.Writer) int {
	if path == "" {
		fmt.Fprintln(stderr, "✗ --worktree needs a path")
		return 2
	}
	target, err := filepath.Abs(path)
	if err == nil {
		target, err = filepath.EvalSymlinks(target)
	}
	if err != nil {
		fmt.Fprintf(stderr, "✗ resolve %s: %v\n", path, err)
		return 3
	}
	trees, err := repoTrees(ctx, target)
	if err != nil {
		// No listing means the repo's checkouts are unknown, and a session
		// loto cannot place could be in any of them.
		fmt.Fprintf(stderr, "✗ list worktrees of %s: %v\n", path, err)
		return 3
	}
	lotoRecs, ccRecs, err := otherSessions()
	if err != nil {
		// Claude Code's records are the witness for a session loto has no
		// record of; unread, any checkout of the repo may hold one.
		fmt.Fprintf(stderr, "✗ read Claude Code sessions: %v\n", err)
		return 3
	}
	sc := presenceScope{Target: target, Trees: trees, Fold: caseInsensitiveFS(target)}
	verdicts := decideWorktreePresence(sc, lotoRecs, ccRecs, canonPath)

	rows := make([]render.PresenceRow, len(verdicts))
	occupied := 0
	for i, v := range verdicts {
		rows[i] = render.PresenceRow{Session: v.Session, PID: v.PID, State: v.State, Source: v.Source, At: v.At, Reason: v.Reason}
		if v.State == presenceOccupied {
			occupied++
		}
	}
	unknown := len(verdicts) - occupied
	render.EmitWorktreePresence(stdout, target, occupied, unknown, rows)
	switch {
	case occupied > 0:
		return 1
	case unknown > 0:
		return 3
	}
	return 0
}

// repoTrees is every checkout of the repo dir belongs to, in canonical form.
func repoTrees(ctx context.Context, dir string) ([]string, error) {
	recs, err := gitWorktrees(ctx, dir)
	if err != nil {
		return nil, err
	}
	trees := make([]string, 0, len(recs))
	for i := range recs {
		if !recs[i].Bare {
			trees = append(trees, canonPath(recs[i].Path))
		}
	}
	return trees, nil
}

// otherSessions reads every loto and Claude Code session record on this host
// but the caller's own, each with its liveness verdict. An unreadable Claude
// Code session dir is an error, not an empty list.
func otherSessions() ([]lotoSession, []claudeSession, error) {
	var lotoRecs []lotoSession
	recs := identity.Sessions()
	for i := range recs {
		if recs[i].IsCaller() {
			continue
		}
		v := recs[i].Verdict()
		lotoRecs = append(lotoRecs, lotoSession{Rec: recs[i], Liveness: v.Liveness, Reason: v.Reason})
	}
	ccs, err := identity.ClaudeSessions()
	if err != nil {
		return nil, nil, err
	}
	var ccRecs []claudeSession
	for _, c := range ccs {
		if c.IsCaller() {
			continue
		}
		ccRecs = append(ccRecs, claudeSession{Rec: c, Liveness: c.Liveness()})
	}
	return lotoRecs, ccRecs, nil
}
