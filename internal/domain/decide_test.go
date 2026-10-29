package domain

import (
	"testing"
	"time"
)

const tcDecideA = "a.go"

// TestDecide_WriteGateFixTable drives Decide through every D row of
// docs/write-gate.md's fix table: each fix the model derives is one case of
// the conflict rule, not a carve-out of its own.
func TestDecide_WriteGateFixTable(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	live := now.Add(time.Hour)
	lapsed := now.Add(-time.Second)
	const me, peer AgentUUID = "me", "peer"
	const wt, other = "/repo", "/repo/.claude/worktrees/b"
	lock := func(owner AgentUUID, worktree, target string, exp time.Time) Hold {
		return EvalContext{Now: now}.LockHold(LockRecord{OwnerUUID: owner, Worktree: worktree, Target: Target{Canonical: target}, ExpiresAt: exp})
	}
	claim := func(owner AgentUUID, worktree, prefix string) Hold {
		return EvalContext{Now: now}.ClaimHold(ClaimRecord{OwnerUUID: owner, Worktree: worktree, PathPrefix: prefix, ExpiresAt: live})
	}
	cases := []struct {
		fix      string
		caseFold bool
		actor    AgentUUID
		worktree string
		target   string
		holds    []Hold
		want     int
	}{
		{fix: "#349 a Write creating its file is admitted: the rule reads the target, not the file",
			actor: me, worktree: wt, target: "new.go", want: -1},
		{fix: "#349 a Write of a file the actor holds is admitted",
			actor: me, worktree: wt, target: "new.go", holds: []Hold{lock(me, wt, "new.go", live)}, want: -1},
		{fix: "#351 one actor is one holder: a hold minted under another session id is its own",
			actor: me, worktree: wt, target: tcDecideA,
			holds: []Hold{EvalContext{Now: now}.LockHold(LockRecord{OwnerUUID: me, SessionUUID: "rotated", Worktree: wt, Target: Target{Canonical: tcDecideA}, ExpiresAt: live})}, want: -1},
		{fix: "#354/#355/#358 a worktree being born has no actor in it",
			actor: me, worktree: other, target: tcDecideA, holds: []Hold{lock(peer, wt, tcDecideA, live)}, want: -1},
		{fix: "#362 a peer's directory hold refuses a hold beneath it",
			actor: me, worktree: wt, target: "pkg/a.go", holds: []Hold{claim(peer, wt, "pkg")}, want: 0},
		{fix: "#362 a peer's hold beneath a directory refuses the directory",
			actor: me, worktree: wt, target: "pkg", holds: []Hold{lock(peer, wt, "pkg/a.go", live)}, want: 0},
		{fix: "#372 a peer's hold in another worktree does not refuse",
			actor: me, worktree: wt, target: tcDecideA, holds: []Hold{lock(peer, other, tcDecideA, live), claim(peer, other, ".")}, want: -1},
		{fix: "#372 a peer's live hold in this worktree refuses",
			actor: me, worktree: wt, target: tcDecideA, holds: []Hold{lock(peer, wt, tcDecideA, live)}, want: 0},
		{fix: "#373 one actor's holds in two worktrees coexist",
			actor: me, worktree: other, target: tcDecideA, holds: []Hold{lock(me, wt, tcDecideA, live)}, want: -1},
		{fix: "#377 the refusing hold is the one in the write's worktree",
			actor: me, worktree: wt, target: tcDecideA, holds: []Hold{lock(peer, other, tcDecideA, live), lock("third", wt, tcDecideA, live)}, want: 1},
		{fix: "#379 a target is case-folded where the filesystem folds case",
			caseFold: true, actor: me, worktree: wt, target: "readme.md", holds: []Hold{lock(peer, wt, "README.md", live)}, want: 0},
		{fix: "#379 a case-sensitive filesystem keeps the two names apart",
			actor: me, worktree: wt, target: "readme.md", holds: []Hold{lock(peer, wt, "README.md", live)}, want: -1},
		{fix: "#394 a lease that lapsed is not live and does not refuse",
			actor: me, worktree: wt, target: tcDecideA, holds: []Hold{lock(peer, wt, tcDecideA, lapsed)}, want: -1},
		{fix: "#403 the actor's holds in every worktree are its own",
			actor: me, worktree: wt, target: tcDecideA, holds: []Hold{lock(me, other, tcDecideA, live), lock(me, wt, tcDecideA, live), claim(me, wt, ".")}, want: -1},
	}
	for _, tc := range cases {
		t.Run(tc.fix, func(t *testing.T) {
			ec := EvalContext{Now: now, CaseFold: tc.caseFold}
			if got := ec.Decide(tc.actor, tc.worktree, tc.target, tc.holds); got != tc.want {
				t.Errorf("Decide = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestRefuses_EmptyActorOwnsNothing: sync overwrites bytes for no actor, so
// every live covering hold refuses it, the caller's own included.
func TestRefuses_EmptyActorOwnsNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	ec := EvalContext{Now: now}
	h := ec.LockHold(LockRecord{OwnerUUID: "", Target: Target{Canonical: tcDecideA}, ExpiresAt: now.Add(time.Hour)})
	if !ec.Refuses("", "", tcDecideA, h) {
		t.Error("a live hold with no owner did not refuse the empty actor")
	}
}
