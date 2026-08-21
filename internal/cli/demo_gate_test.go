//go:build unix

package cli

// Demo 21 — the git-gate end to end (loto-ovno.11).
//
// Everything before this file demos the coordination layer: locks, intents,
// tags. This one demos what the locks are FOR — two agents landing real edits
// on refs/loto/integration from one shared checkout, a rogue write that never
// gets in, a candidate that verify rejects, and `loto sync` cleaning up after
// both.
//
// Tagged unix because gate.Promote is (its flock is syscall.Flock).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"loto/internal/gate"
)

const (
	dgRogueFile = "rogue.go"
	dgRedFile   = "red.go"
	dgBeadA     = "loto-ovno.11a"
	dgBeadB     = "loto-ovno.11b"
	dgBeadC     = "loto-ovno.11c"
)

// TestDemo_21_GateEndToEnd walks git-gate.md Outcomes 1–3 in one run.
//
// It is a demo, so it narrates. It is also the bead's acceptance criterion, so
// it asserts — every claim on screen is checked, and the checks are the point:
//
//	Outcome 1  a rogue write to an unleased path never reaches integration,
//	           and no candidate launders it.
//	Outcome 2  two agents fork one base and land disjoint write-sets from one
//	           checkout, each promoted commit carrying its own attribution.
//	Outcome 3  verify judges the PROSPECTIVE integration state in isolation —
//	           proven by leaving the rogue edit in the working tree while a
//	           verify that would reject it passes.
//
// Plus the batching path (two candidates, one chain, ONE verify) and `loto
// sync` repairing the residue a rejected candidate leaves in the tree.
func TestDemo_21_GateEndToEnd(t *testing.T) {
	head(t, 21, "git-gate — two agents, one rogue write, sync repairs residue")
	repo := demoGateRepo(t)
	alice, bob, carol := triCast(t)
	verify, runs := demoVerifyScript(t)

	// ── act 1 · two agents, one checkout, disjoint write-sets ──────────────
	say(t, "alice and bob share one working tree. no worktrees, no branch dance.")
	say(t, "each locks its own file, edits it, and submits a candidate.")
	beat(t)
	alice.do(t, "lock", tcTargetA, "--intent", "lane: a.go")
	demoWrite(t, repo, tcTargetA, "package main // alice\n")
	_, outA := alice.do(t, "submit", tcTargetA, "--bead", dgBeadA)
	mustContain(t, outA, "✓ candidate id=c-")
	beat(t)
	bob.do(t, "lock", tcStoreStoreGo, "--intent", "lane: store.go")
	demoWrite(t, repo, tcStoreStoreGo, "package store // bob\n")
	_, outB := bob.do(t, "submit", tcStoreStoreGo, "--bead", dgBeadB)
	mustContain(t, outB, "✓ candidate id=c-")
	beat(t)
	say(t, "two candidates queued under refs/loto/candidates/. neither has")
	say(t, "touched integration yet — a candidate is a proposal, not a landing.")
	beat(t)

	// ── act 2 · the rogue write ────────────────────────────────────────────
	say(t, "meanwhile something writes rogue.go out of band. no lock, no")
	say(t, "candidate, no attribution — the classic `sed -i` from a stray tool.")
	beat(t)
	demoWrite(t, repo, dgRogueFile, "package main // ROGUE — nobody claimed this\n")
	t.Logf("    %-10s ❯ sed -i 's/.../ROGUE/' %s", "(rogue)", dgRogueFile)
	beat(t)

	// ── act 3 · promotion: batched, verified in isolation, attributed ──────
	before := demoIntegration(t, repo)
	say(t, "the pusher promotes. both candidates chain into ONE prospective")
	say(t, "state and get ONE verify — that is the batching path.")
	say(t, "the verify command rejects any tree containing ROGUE.")
	beat(t)
	res := demoPromote(t, repo, verify)
	for _, o := range res.Outcomes {
		t.Logf("    %-10s   ✓ %s class=%s bead=%s", "(gate)", o.CandidateID, o.Class, o.BeadID)
	}
	beat(t)

	if n := len(res.Outcomes); n != 2 {
		t.Fatalf("want 2 outcomes from the batch, got %d: %+v", n, res.Outcomes)
	}
	for _, o := range res.Outcomes {
		if o.Class != gate.OutcomePromoted {
			t.Fatalf("%s: class=%s, want promoted", o.CandidateID, o.Class)
		}
	}
	// Batching: two candidates, one verify. If this ever reads 2, the chain
	// stopped batching and the loto-ovno.1 latency budget silently doubled.
	if got := demoRunCount(t, runs); got != 1 {
		t.Errorf("batching: %d verify runs for a 2-candidate batch, want 1", got)
	}
	// Outcome 3: that single verify PASSED while rogue.go sat modified in the
	// shared tree. It can only have passed by reading the prospective
	// integration state out of a cut worktree — the tree it would have read
	// instead still says ROGUE.
	if !strings.Contains(demoRead(t, repo, dgRogueFile), "ROGUE") {
		t.Fatal("precondition: the rogue edit must still be in the working tree")
	}
	say(t, "✓ verify passed — while ROGUE was sitting in the shared tree.")
	say(t, "  it never saw the tree. it saw the prospective integration state,")
	say(t, "  cut into its own worktree. in-tree results are diagnostic only.")
	beat(t)

	// Outcome 2: both landed, each commit naming its own author and bead.
	after := demoIntegration(t, repo)
	msgs := submitGitT(t, repo, "log", "--format=%B", before+".."+after)
	if n := len(strings.Split(submitGitT(t, repo, "rev-list", before+".."+after), "\n")); n != 2 {
		t.Errorf("integration advanced by %d commits, want 2", n)
	}
	for _, want := range []string{"Bead: " + dgBeadA, "Bead: " + dgBeadB, "Agent: " + alice.agent.UUID, "Agent: " + bob.agent.UUID} {
		if !strings.Contains(msgs, want) {
			t.Errorf("promoted commits must carry individual attribution, missing %q", want)
		}
	}
	say(t, "✓ two commits on refs/loto/integration, one per agent, each")
	say(t, "  carrying its own Agent/Session/Bead trailers.")
	beat(t)

	// Outcome 1: the rogue write is not on integration and never was.
	if got := submitGitT(t, repo, "show", after+":"+dgRogueFile); strings.Contains(got, "ROGUE") {
		t.Fatalf("a rogue write reached integration: %q", got)
	}
	say(t, "✓ rogue.go on integration is untouched. the candidate write-set is")
	say(t, "  exact, so an unleased edit has no way in — it can only sit in the")
	say(t, "  tree as residue.")
	beat(t)

	// ── act 4 · a candidate verify rejects ─────────────────────────────────
	say(t, "carol submits a change that breaks the invariant (BROKEN).")
	say(t, "admission accepts it — admission checks authority, not behavior.")
	beat(t)
	carol.do(t, "lock", dgRedFile, "--intent", "lane: red.go")
	demoWrite(t, repo, dgRedFile, "package main // BROKEN\n")
	_, outC := carol.do(t, "submit", dgRedFile, "--bead", dgBeadC)
	mustContain(t, outC, "✓ candidate id=c-")
	beat(t)
	say(t, "the pusher promotes again. verify says no.")
	beat(t)
	red := demoPromote(t, repo, verify)
	for _, o := range red.Outcomes {
		t.Logf("    %-10s   ✗ %s class=%s bead=%s", "(gate)", o.CandidateID, o.Class, o.BeadID)
	}
	beat(t)
	if len(red.Outcomes) != 1 || red.Outcomes[0].Class != gate.OutcomeVerifyRed {
		t.Fatalf("want one verify-red outcome, got %+v", red.Outcomes)
	}
	if red.Outcomes[0].BeadID != dgBeadC {
		t.Errorf("a rejection must name its bead, got %q", red.Outcomes[0].BeadID)
	}
	if got := demoIntegration(t, repo); got != after {
		t.Errorf("integration moved on a rejected candidate: %s -> %s", after, got)
	}
	say(t, "✗ rejected, attributed to carol's bead. integration did not move.")
	say(t, "  but carol's edit is still sitting in the shared tree — residue.")
	beat(t)

	// ── act 5 · sync repairs the residue ───────────────────────────────────
	say(t, "carol releases the lease; sync will not overwrite a leased path.")
	beat(t)
	carol.do(t, "unlock", dgRedFile, "-t", "candidate rejected")
	beat(t)
	say(t, "two divergent paths now: carol's rejected edit and the rogue write.")
	say(t, "loto sync fast-forwards both back to integration content.")
	beat(t)
	_, outS := carol.do(t, "sync")
	mustContain(t, outS, "synced=2")
	beat(t)
	for _, f := range []string{dgRedFile, dgRogueFile} {
		if got := demoRead(t, repo, f); strings.Contains(got, "BROKEN") || strings.Contains(got, "ROGUE") {
			t.Errorf("%s: sync left residue behind: %q", f, got)
		}
	}
	// alice's and bob's promoted content must survive the repair untouched —
	// sync fast-forwards divergence, it does not rewind integrated work.
	if got := demoRead(t, repo, tcTargetA); !strings.Contains(got, "alice") {
		t.Errorf("sync clobbered promoted content in %s: %q", tcTargetA, got)
	}
	say(t, "✓ tree matches integration again. alice's and bob's landed work is")
	say(t, "  untouched — sync repairs divergence, it never rewinds what landed.")
}

// ─── act support ────────────────────────────────────────────────────────────

// demoGateRepo builds the demo's starting state: the standard temp project,
// plus the two extra tracked files acts 2 and 4 dirty, all committed so
// refs/loto/integration has somewhere to bootstrap from.
func demoGateRepo(t *testing.T) string {
	t.Helper()
	repo := withTempProject(t)
	for _, f := range []string{dgRogueFile, dgRedFile} {
		if err := os.WriteFile(filepath.Join(repo, f), []byte("package main\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	submitGitT(t, repo, "add", "-A")
	submitGitT(t, repo, "commit", "-q", "-m", "base")
	return repo
}

// demoVerifyScript writes the gate-owned invariant command and returns it plus
// the path it records each invocation to.
//
// ‡ The invariant is deliberately a content check, not a test run: it must be
// able to fail on a specific candidate's change (BROKEN) and on tree residue
// that no candidate declared (ROGUE), so the demo can show the difference
// between the two. Counting runs is what makes the batching claim checkable.
func demoVerifyScript(t *testing.T) (cmd []string, runs string) {
	t.Helper()
	dir := t.TempDir()
	runs = filepath.Join(dir, "verify-runs")
	script := filepath.Join(dir, "verify.sh")
	body := "#!/bin/sh\n" +
		"echo run >> " + runs + "\n" +
		"if grep -q ROGUE " + dgRogueFile + "; then exit 1; fi\n" +
		"if grep -q BROKEN " + dgRedFile + "; then exit 1; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return []string{script}, runs
}

func demoRunCount(t *testing.T, runs string) int {
	t.Helper()
	b, err := os.ReadFile(runs)
	if err != nil {
		t.Fatalf("verify never ran: %v", err)
	}
	return len(strings.Fields(string(b)))
}

// demoPromote runs one promotion in this process, the way a pusher would — no
// daemon, the pusher's own process (git-gate.md non-goals).
//
// ‡ Promotion is library-only. No CLI verb advances refs/loto/integration, and
// loto-ovno.8's `loto pr` is not one either: it reads whatever integration
// already holds and bridges it onward to per-bead branches and PRs. So the demo
// calls gate.Promote directly. If a promote verb ever lands, this function and
// the act-3 narration are the two places to swap it in.
func demoPromote(t *testing.T, repo string, verify []string) gate.PromoteResult {
	t.Helper()
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	res, err := gate.Promote(rt.Ctx, gate.PromoteParams{
		RepoTop: repo, VerifyCmd: verify, Claims: rt.Store,
		Host: rt.Host, PID: os.Getpid(),
	})
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	return res
}

func demoIntegration(t *testing.T, repo string) string {
	t.Helper()
	return submitGitT(t, repo, "rev-parse", gate.IntegrationRef)
}

func demoWrite(t *testing.T, repo, rel, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, rel), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func demoRead(t *testing.T, repo, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repo, rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
