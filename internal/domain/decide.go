package domain

// Hold is the one shape the write gate's conflict rule reads (docs/write-gate.md):
// whose it is, which worktree, which target, and whether it is live. A lock and
// a claim both lift into it — a lock holds one path, a claim a directory
// prefix — so every surface that decides a write asks one question of one
// shape. Live is settled when the hold is lifted (LockHold, ClaimHold), against
// the same EvalContext that then decides.
type Hold struct {
	Actor    AgentUUID
	Worktree string
	Target   string
	Live     bool
}

// LockHold lifts a lock row into a Hold. Live is !IsStale: TTL lapse or a
// provably dead holder frees the territory.
func (c EvalContext) LockHold(l LockRecord) Hold {
	return Hold{Actor: l.OwnerUUID, Worktree: l.Worktree, Target: l.Target.Canonical, Live: !c.IsStale(l)}
}

// ClaimHold lifts a claim into a Hold. ClaimIsStale applies the lock's own
// standard of live to the claim (loto-tzmv.9).
func (c EvalContext) ClaimHold(cl ClaimRecord) Hold {
	return Hold{Actor: cl.OwnerUUID, Worktree: cl.Worktree, Target: cl.PathPrefix, Live: !c.ClaimIsStale(cl)}
}

// Covers is the territory half of the conflict rule: h lies in worktree and
// its target covers target or is covered by it, under this context's case
// class. A directory target covers every path beneath it. "" for either
// worktree widens (SameWorktree).
func (c EvalContext) Covers(worktree, target string, h Hold) bool {
	return SameWorktree(worktree, h.Worktree) && c.PrefixCovers(h.Target, target)
}

// Refuses is the conflict rule, and the only predicate in loto that decides a
// write: actor in worktree may not hold or write target while a live hold of
// another actor in that worktree covers target or is covered by it.
// An empty actor owns nothing, so every live
// covering hold refuses it.
func (c EvalContext) Refuses(actor AgentUUID, worktree, target string, h Hold) bool {
	if actor != "" && h.Actor == actor {
		return false
	}
	return h.Live && c.Covers(worktree, target, h)
}

// Decide is Refuses over a set of holds: the index of the first hold that
// refuses actor's write of target in worktree, or -1 to admit. Callers pass
// only the holds that cover the target (store.LocksAt is that indexed read),
// so the hook's decision costs one lookup, not a scan of every hold.
func (c EvalContext) Decide(actor AgentUUID, worktree, target string, holds []Hold) int {
	for i := range holds {
		if c.Refuses(actor, worktree, target, holds[i]) {
			return i
		}
	}
	return -1
}
