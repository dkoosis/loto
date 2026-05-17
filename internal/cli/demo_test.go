package cli

// Demo tests — animated, play-shaped walk-throughs of every loto primitive
// AND the multi-agent coordination patterns Claude sessions use day to day.
//
// Run with:
//
//	go test -v -run Demo ./internal/cli
//
// Each test is a tiny play. Narration sits to the left of the screen; commands
// are prefixed with the actor's handle and a `❯` prompt; output shows only the
// key line. Read top-to-bottom, you watch agents (alice, bob, carol) negotiate
// territory through loto.
//
// Layout:
//   00      index (table of contents — declared first so `go test -v` shows it first)
//   01-08   primitives          (whoami, lock+status, check, racing, handoff, status, doctor, version)
//   09-16   coordination        (lane, drain, hook-gate, migration, queue, reviewer, TDD, cross-repo)
//   17-20   safety invariants   (multi-file atomic, reads-are-free, force-break, lazy GC)

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"loto/internal/identity"
)

const demoWidth = 72

// ─── stage helpers ───────────────────────────────────────────────────────────

func head(t *testing.T, n int, title string) {
	t.Helper()
	line := fmt.Sprintf("── demo %02d · %s ", n, title)
	if len(line) < demoWidth {
		line += strings.Repeat("─", demoWidth-len(line))
	}
	t.Logf("\n%s", line)
}

// say emits a single narration line (no actor, no prompt).
func say(t *testing.T, line string) {
	t.Helper()
	t.Logf("    %s", line)
}

// beat emits a blank line for breathing room.
func beat(t *testing.T) {
	t.Helper()
	t.Log("")
}

// actor is one performer in a demo. The Setenv pin survives until another
// actor steps up or the test ends.
type actor struct {
	handle string
	agent  *identity.Agent
}

func cast(t *testing.T) (alice, bob actor) {
	t.Helper()
	a, b := twoAgents(t)
	return actor{handle: a.Handle, agent: a}, actor{handle: b.Handle, agent: b}
}

func solo(t *testing.T) actor {
	t.Helper()
	a := pinAgent(t)
	return actor{handle: a.Handle, agent: a}
}

// triCast mints a third actor (carol) alongside alice + bob, for demos that
// need three peers (e.g. parallel feature lanes).
func triCast(t *testing.T) (alice, bob, carol actor) {
	t.Helper()
	a, b := cast(t)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", fmt.Sprintf("carol-%d", time.Now().UnixNano()))
	c, err := identity.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	return a, b, actor{handle: c.Handle, agent: c}
}

// tryDo runs a loto command as the given actor and renders it as a single
// prompt line plus one collapsed result line. Returns exit + the captured
// body (stdout, or stderr when stdout is empty — matches what the demo log
// renders) without asserting; use for demos that expect a non-zero exit.
// Returning the same body the logger shows means error-path demos can assert
// on messages the user sees, which often land on stderr (e.g. blocker= rows
// from `loto check` exit-1).
func (a actor) tryDo(t *testing.T, args ...string) (int, string) {
	t.Helper()
	t.Setenv("LOTO_AGENT_ID", a.agent.UUID)

	var out, errBuf bytes.Buffer
	code := Run(args, &out, &errBuf)
	body := strings.TrimRight(out.String(), "\n")
	if body == "" {
		body = strings.TrimRight(errBuf.String(), "\n")
	}

	cmd := "loto " + strings.Join(args, " ")
	t.Logf("    %-10s ❯ %s", a.handle, cmd)

	for ln := range strings.SplitSeq(body, "\n") {
		if strings.TrimSpace(ln) == "" {
			continue
		}
		t.Logf("    %-10s   %s", "", ln)
	}
	t.Logf("    %-10s   (exit %d)", "", code)
	return code, body
}

// do is tryDo plus an assertion that the command exited zero. Surprise
// non-zero exits fail the demo loudly instead of being swallowed.
func (a actor) do(t *testing.T, args ...string) (int, string) {
	t.Helper()
	code, out := a.tryDo(t, args...)
	if code != 0 {
		t.Fatalf("loto %s: expected exit 0, got %d", strings.Join(args, " "), code)
	}
	return code, out
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Fatalf("expected output to contain %q, got:\n%s", want, got)
	}
}

// touch creates an empty file at repo-relative path, making parent dirs as
// needed. AcquireLocks Lstat-validates KindFile targets so demo files must
// exist on disk.
func touch(t *testing.T, repo, rel string) string {
	t.Helper()
	full := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

// ─── 00 · index ──────────────────────────────────────────────────────────────
//
// Declared first so `go test -v` (which runs in source order) shows it before
// the other demos. No assertions — pure narration.

func TestDemo_00_Index(t *testing.T) {
	rows := []string{
		"── primitives ──",
		"01  whoami           identity of the current session",
		"02  lock + status    claim a file, inspect what you hold",
		"03  check            pre-flight before editing",
		"04  contention       two agents racing for the same file",
		"05  handoff          unlock so a peer can claim",
		"06  status (global)  every active lock, across agents",
		"07  doctor           find and repair stale locks",
		"08  version          meta",
		"── coordination patterns ──",
		"09  lane by anchors  parallel feature lanes without globs",
		"10  hook gate        pre-write check loop, skip on conflict",
		"11  drain loop       claim → edit → unlock, three files in a row",
		"12  migration TTL    long claim with intent that explains itself",
		"13  queue behind     poll check until peer releases",
		"14  reviewer peek    status as in-flight signal, not as block",
		"15  TDD ping-pong    red / green / refactor tags drive the handoff",
		"16  cross-repo note  lock domain is per-project — by design",
		"── safety invariants ──",
		"17  multi-file atomic  lock a+b+c all-or-nothing; one blocker aborts the set",
		"18  reads are free     locked file is 0444 — peers can still read",
		"19  force-break        unlock --force takes over; no silent dispossession",
		"20  lazy GC            expired row reaped on next acquire, no doctor needed",
	}
	var b strings.Builder
	b.WriteString("\n    ┌─ loto demo index ")
	b.WriteString(strings.Repeat("─", demoWidth-len("    ┌─ loto demo index ")-1))
	b.WriteString("┐\n")
	for _, r := range rows {
		b.WriteString("    │  ")
		b.WriteString(r)
		pad := demoWidth - len(r) - 8
		if pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString("│\n")
	}
	b.WriteString("    └")
	b.WriteString(strings.Repeat("─", demoWidth-5))
	b.WriteString("┘\n")
	t.Log(b.String())
}

// ─── 01-08 · primitives ──────────────────────────────────────────────────────

func TestDemo_01_WhoAmI(t *testing.T) {
	head(t, 1, "whoami — every session has a handle")
	withTempProject(t)
	a := solo(t)

	say(t, "a fresh session lands in the repo.")
	say(t, "first question: what am I called here?")
	beat(t)
	_, out := a.do(t, "whoami")
	mustContain(t, out, "handle:")
	beat(t)
	say(t, "that handle is what peers will see in conflict reports.")
}

func TestDemo_02_LockAndStatus(t *testing.T) {
	head(t, 2, "lock + status — claim, then inspect")
	withTempProject(t)
	a := solo(t)

	say(t, a.handle+" is about to refactor a.go. she locks it first.")
	beat(t)
	_, out := a.do(t, "lock", "a.go", "--intent", "rename Type")
	mustContain(t, out, "✓ locked")
	beat(t)
	say(t, "what does she hold right now?")
	beat(t)
	_, out = a.do(t, "status", "--mine")
	mustContain(t, out, "a.go")
}

func TestDemo_03_CheckBeforeEdit(t *testing.T) {
	head(t, 3, "check — pre-flight before you touch a file")
	repo := withTempProject(t)
	touch(t, repo, "b.go")
	alice, bob := cast(t)

	say(t, alice.handle+" locks b.go for a fix.")
	beat(t)
	alice.do(t, "lock", "b.go", "--intent", "fix race")
	beat(t)
	say(t, bob.handle+" wanders in, doesn't know yet, and runs check first.")
	beat(t)
	code, out := bob.tryDo(t, "check", "b.go")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	mustContain(t, out, "blocker=")
	mustContain(t, out, `intent="fix race"`)
	beat(t)
	say(t, "exit 1 + blocker= line says: do NOT edit.")
	say(t, "the intent tells "+bob.handle+" what the holder is doing — enough to route around.")
}

func TestDemo_04_TwoAgentsRacing(t *testing.T) {
	head(t, 4, "contention — two agents want the same file")
	withTempProject(t)
	alice, bob := cast(t)

	say(t, alice.handle+" gets there first.")
	beat(t)
	_, out := alice.do(t, "lock", "internal/store/store.go", "--intent", "split file")
	mustContain(t, out, "✓ locked")
	beat(t)
	say(t, bob.handle+" tries the same file moments later.")
	beat(t)
	code, out := bob.tryDo(t, "lock", "internal/store/store.go", "--intent", "add test")
	if code == 0 {
		t.Fatalf("expected blocked, got exit 0")
	}
	mustContain(t, out, "✗ blocked")
	beat(t)
	say(t, "✗ blocked — this is the whole point of loto. no silent clobbers.")
}

func TestDemo_05_HandoffViaUnlock(t *testing.T) {
	head(t, 5, "unlock — hand the territory back")
	withTempProject(t)
	alice, bob := cast(t)

	say(t, alice.handle+" claims the file, does the work.")
	beat(t)
	alice.do(t, "lock", "internal/store/store.go", "--intent", "split file")
	beat(t)
	say(t, "she's done. the tag explains WHY she's releasing — peers see it in history.")
	beat(t)
	_, out := alice.do(t, "unlock", "internal/store/store.go", "-t", "ready-for-review")
	mustContain(t, out, "✓ unlocked")
	beat(t)
	say(t, bob.handle+" can now claim what was blocked a moment ago.")
	beat(t)
	_, out = bob.do(t, "lock", "internal/store/store.go", "--intent", "add test")
	mustContain(t, out, "✓ locked")
}

func TestDemo_06_GlobalStatus(t *testing.T) {
	head(t, 6, "status — full picture across agents")
	withTempProject(t)
	alice, bob := cast(t)

	say(t, "two agents working in parallel each claim a file.")
	beat(t)
	alice.do(t, "lock", "a.go", "--intent", "doc pass")
	bob.do(t, "lock", "internal/store/store.go", "--intent", "extract helper")
	beat(t)
	say(t, "a third session walks in and wants the lay of the land.")
	beat(t)
	c := solo(t)
	c.do(t, "status")
	beat(t)
	say(t, "no --mine = every active lock in the repo, with owner UUIDs + intent.")
}

func TestDemo_07_DoctorStaleLock(t *testing.T) {
	head(t, 7, "doctor — find and repair stale locks")
	withTempProject(t)
	a := solo(t)

	say(t, "a previous session locked the file with a short TTL and never came back.")
	say(t, "TTL expired with no clean unlock — same shape as a crash.")
	beat(t)
	a.do(t, "lock", "a.go", "--ttl", "1ms", "--intent", "interrupted")
	time.Sleep(50 * time.Millisecond)
	beat(t)
	say(t, "dry-run first — see the plan before mutating state.")
	beat(t)
	a.do(t, "doctor", "--dry-run", "--repair")
	beat(t)
	say(t, "now for real.")
	beat(t)
	a.do(t, "doctor", "--repair")
	beat(t)
	say(t, "doctor keeps the lock table honest. run it when the repo feels sticky.")
}

func TestDemo_08_Version(t *testing.T) {
	head(t, 8, "version — meta")
	withTempProject(t)
	a := solo(t)
	say(t, "loto rev= pins the binary commit. when peers disagree about a lock,")
	say(t, "first thing to compare: are we even on the same loto?")
	beat(t)
	_, out := a.do(t, "version")
	mustContain(t, out, "loto rev=")
	mustContain(t, out, "built=")
}

// ─── 09-16 · coordination patterns ───────────────────────────────────────────
//
// Each pattern teaches a multi-agent recipe Claude sessions hit in practice.
// They use only the seven primitives demoed above — no vapor commands.

// TestDemo_09_LaneByAnchors — UC1: parallel feature lanes.
//
// loto intentionally cut directory/glob locks (PR #65). To claim a subtree,
// hold the *anchor* file (registry, entry point) with an intent that names
// the lane. Peers reading status see the lane and route around it.
func TestDemo_09_LaneByAnchors(t *testing.T) {
	head(t, 9, "lane by anchors — own a subtree without globs")
	repo := withTempProject(t)
	touch(t, repo, "internal/auth/auth.go")
	touch(t, repo, "internal/api/api.go")
	alice, bob, carol := triCast(t)

	say(t, "three agents divide the repo into feature lanes.")
	say(t, "loto doesn't lock subtrees, so each claims the anchor file with intent.")
	beat(t)
	alice.do(t, "lock", "internal/store/store.go", "--intent", "lane: store/** refactor")
	bob.do(t, "lock", "internal/auth/auth.go", "--intent", "lane: auth/** session work")
	carol.do(t, "lock", "internal/api/api.go", "--intent", "lane: api/** endpoint adds")
	beat(t)
	say(t, "a fourth session walks in and runs status. one glance = whole map.")
	beat(t)
	d := solo(t)
	_, out := d.do(t, "status")
	mustContain(t, out, "lane:")
	beat(t)
	say(t, "the lane intents tell the newcomer where NOT to wander.")
	say(t, "convention does the work that globs would in a heavier tool.")
}

// TestDemo_10_HookGate — UC2: the pre-write reflex.
//
// loto has no `loto hook` subcommand — the gate is a one-liner the agent runs
// before every write. Claude Code hooks call this in PreToolUse so the agent
// never forgets.
func TestDemo_10_HookGate(t *testing.T) {
	head(t, 10, "hook gate — check before every write")
	repo := withTempProject(t)
	touch(t, repo, "owned.go")
	touch(t, repo, "free.go")
	alice, bob := cast(t)

	alice.do(t, "lock", "owned.go", "--intent", "iterating")
	beat(t)
	say(t, bob.handle+" is about to edit two files. policy: check first, every time.")
	beat(t)
	say(t, "→ free.go: clear, proceed.")
	beat(t)
	_, out := bob.do(t, "check", "free.go")
	mustContain(t, out, "✓ no conflicts")
	beat(t)
	say(t, "→ owned.go: blocked. skip, log, move on.")
	beat(t)
	code, _ := bob.tryDo(t, "check", "owned.go")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	beat(t)
	say(t, "exit 0 = green light. exit 1 = peer holds it. wire `check` into")
	say(t, "PreToolUse and the gate runs itself.")
}

// TestDemo_11_DrainLoop — UC2: backlog drain across multiple files.
//
// The shape of /team backlog: pick file, claim, edit, release, next. Short
// claims, short intents, fast turnover. Demo runs the loop for three files.
func TestDemo_11_DrainLoop(t *testing.T) {
	head(t, 11, "drain loop — claim → edit → unlock, three in a row")
	repo := withTempProject(t)
	files := []string{"f1.go", "f2.go", "f3.go"}
	for _, f := range files {
		touch(t, repo, f)
	}
	a := solo(t)

	say(t, a.handle+" pulls three small tasks off the backlog.")
	say(t, "for each: claim → (edit) → unlock with a tag. no held state between items.")
	beat(t)
	for _, f := range files {
		a.do(t, "lock", f, "--ttl", "5m", "--intent", "drain: "+f)
		a.do(t, "unlock", f, "-t", "done")
	}
	beat(t)
	say(t, "short TTL says 'I'll be quick.' short intent says 'this is what.'")
	say(t, "tagged unlocks leave breadcrumbs in the audit trail.")
	beat(t)
	_, out := a.do(t, "status", "--mine")
	mustContain(t, out, "no locks")
}

// TestDemo_12_MigrationTTL — UC3: long claim that explains itself.
//
// The migration agent isn't blocking maliciously — it's doing real work over
// hours. A long TTL + a clear intent lets peers make sane routing decisions
// without paging a human.
func TestDemo_12_MigrationTTL(t *testing.T) {
	head(t, 12, "migration TTL — long claim that explains itself")
	repo := withTempProject(t)
	touch(t, repo, "internal/api/api.go")
	alice, bob := cast(t)

	say(t, alice.handle+" starts a repo-wide rename. she expects to be a while.")
	beat(t)
	alice.do(t, "lock", "internal/store/store.go",
		"--ttl", "4h",
		"--intent", "migration: rename Foo→Bar across internal/store/**")
	beat(t)
	say(t, bob.handle+" needs to touch store.go for a bugfix. checks first.")
	beat(t)
	code, out := bob.tryDo(t, "check", "internal/store/store.go")
	if code != 1 {
		t.Fatalf("expected exit 1, got %d", code)
	}
	mustContain(t, out, "migration:")
	beat(t)
	say(t, "the intent reads 'migration … hours.' "+bob.handle+" reroutes to api.go instead.")
	beat(t)
	bob.do(t, "lock", "internal/api/api.go", "--intent", "bugfix: nil deref in handler")
	beat(t)
	say(t, "no human paged. no force-break. the intent string did the routing.")
}

// TestDemo_13_QueueBehind — UC3: poll-check until clear.
//
// loto has no `wait` primitive yet — the idiom is `until loto check; do
// sleep; done`. Demo shows the shape: bob polls, alice releases, bob proceeds.
func TestDemo_13_QueueBehind(t *testing.T) {
	head(t, 13, "queue behind — poll check until peer releases")
	repo := withTempProject(t)
	touch(t, repo, "hot.go")
	alice, bob := cast(t)

	alice.do(t, "lock", "hot.go", "--intent", "quick fix, ~1 min")
	beat(t)
	say(t, bob.handle+" must touch hot.go and is willing to wait. she polls.")
	beat(t)
	code, _ := bob.tryDo(t, "check", "hot.go")
	if code != 1 {
		t.Fatalf("first poll: expected exit 1, got %d", code)
	}
	beat(t)
	say(t, "still held. (in real life: sleep 5; check; repeat.)")
	say(t, alice.handle+" finishes and unlocks.")
	beat(t)
	alice.do(t, "unlock", "hot.go", "-t", "done")
	beat(t)
	say(t, "next poll comes back clean. "+bob.handle+" claims and proceeds.")
	beat(t)
	_, out := bob.do(t, "check", "hot.go")
	mustContain(t, out, "✓ no conflicts")
	bob.do(t, "lock", "hot.go", "--intent", "follow-up")
	beat(t)
	say(t, "tighter shell shape (atomic, no TOCTOU between check and lock):")
	say(t, "  while ! loto lock hot.go --intent \"...\"; do sleep 5; done")
	say(t, "→ no `loto wait` primitive needed — `lock` already returns ✗ blocked atomically.")
}

// TestDemo_14_ReviewerPeek — UC4: status as in-flight signal.
//
// Reviewers don't need locks — they just need to know not to file comments
// against a file that's actively churning. `status <file>` is the signal.
func TestDemo_14_ReviewerPeek(t *testing.T) {
	head(t, 14, "reviewer peek — status as in-flight signal")
	repo := withTempProject(t)
	touch(t, repo, "feature.go")
	author, reviewer := cast(t)

	say(t, author.handle+" is iterating on feature.go. lock makes it visible.")
	beat(t)
	author.do(t, "lock", "feature.go", "--intent", "iterating on PR review fixes")
	beat(t)
	say(t, reviewer.handle+" (reviewer) peeks at the file before writing comments.")
	beat(t)
	_, out := reviewer.do(t, "status", "feature.go")
	mustContain(t, out, "iterating")
	beat(t)
	say(t, "file is mid-edit. reviewer holds comments — they'd be stale in 60s.")
	say(t, author.handle+" finishes and unlocks.")
	beat(t)
	author.do(t, "unlock", "feature.go", "-t", "ready-for-review")
	beat(t)
	_, out = reviewer.do(t, "status", "feature.go")
	mustContain(t, out, "✓ free")
	beat(t)
	say(t, "now safe to review. the unlock tag even says so.")
}

// TestDemo_15_TDDPingPong — UC5: red/green/refactor tags drive the handoff.
//
// The tag isn't decoration — it tells the peer what phase to pick up.
func TestDemo_15_TDDPingPong(t *testing.T) {
	head(t, 15, "TDD ping-pong — red / green / refactor")
	repo := withTempProject(t)
	touch(t, repo, "calc.go")
	touch(t, repo, "calc_test.go")
	alice, bob := cast(t)

	say(t, alice.handle+" writes a failing test. intent names the phase.")
	beat(t)
	alice.do(t, "lock", "calc_test.go", "--intent", "RED: failing test for Add")
	alice.do(t, "unlock", "calc_test.go", "-t", "red")
	beat(t)
	say(t, bob.handle+" sees -t red in history and picks up the impl side.")
	beat(t)
	bob.do(t, "lock", "calc.go", "--intent", "GREEN: make Add pass")
	bob.do(t, "unlock", "calc.go", "-t", "green")
	beat(t)
	say(t, "tests pass. "+alice.handle+" reclaims for refactor.")
	beat(t)
	alice.do(t, "lock", "calc.go", "--intent", "REFACTOR: extract helper")
	alice.do(t, "unlock", "calc.go", "-t", "refactor")
	beat(t)
	say(t, "tags + intents make the TDD cycle legible without a human in the loop.")
}

// TestDemo_16_CrossRepoNote — UC6: lock domain is per-project, by design.
//
// loto's state dir is keyed by project slug (git origin or dir basename).
// Worktrees of the same repo share. Sibling checkouts of different repos do
// not. This demo documents the boundary — no assertions, just narration.
func TestDemo_16_CrossRepoNote(t *testing.T) {
	head(t, 16, "cross-repo — lock domain is per-project, by design")
	withTempProject(t)
	a := solo(t)

	say(t, "loto's lock DB lives at $XDG_STATE_HOME/loto/projects/<slug>/.")
	say(t, "<slug> derives from git origin (or dir basename for un-remoted repos).")
	beat(t)
	a.do(t, "status")
	beat(t)
	say(t, "worktrees of the SAME repo share a DB (via GIT_COMMON_DIR).")
	say(t, "sibling checkouts of DIFFERENT repos do NOT share — separate slugs.")
	beat(t)
	say(t, "cross-repo refactors? coordinate out-of-band (chat, shared bead, ticket).")
	say(t, "this is a deliberate scope choice — a single global lock domain would")
	say(t, "couple unrelated projects and turn doctor into a cross-repo concern.")
}

// ─── 17-20 · safety invariants ───────────────────────────────────────────────
//
// These demos exercise load-bearing claims from NORTH_STAR.md that the
// primitives + coordination demos imply but don't actually prove on screen.

// TestDemo_17_MultiFileAtomic — NS invariant: multi-file lock is all-or-nothing.
//
// Any blocker aborts the set: no chmod side effects, no rows inserted.
// This is what makes mid-sweep refactors safe — the changed file set lands
// or doesn't, never partially.
func TestDemo_17_MultiFileAtomic(t *testing.T) {
	head(t, 17, "multi-file atomic — one blocker aborts the whole set")
	repo := withTempProject(t)
	for _, f := range []string{"a.go", "b.go", "c.go"} {
		touch(t, repo, f)
	}
	alice, bob := cast(t)

	say(t, alice.handle+" claims three files in a single invocation.")
	beat(t)
	_, out := alice.do(t, "lock", "a.go", "b.go", "c.go", "--intent", "rename across set")
	mustContain(t, out, "✓ locked")
	beat(t)
	say(t, alice.handle+" releases the set so the next part of the demo is clean.")
	alice.do(t, "unlock", "a.go", "b.go", "c.go", "-t", "done")
	beat(t)
	say(t, "now the harder case: "+bob.handle+" already holds b.go.")
	beat(t)
	bob.do(t, "lock", "b.go", "--intent", "tweak")
	beat(t)
	say(t, alice.handle+" tries the same three-file claim. b.go is blocked.")
	beat(t)
	code, out := alice.tryDo(t, "lock", "a.go", "b.go", "c.go", "--intent", "rename across set")
	if code == 0 {
		t.Fatalf("expected blocked on b.go, got exit 0")
	}
	mustContain(t, out, "✗ blocked")
	beat(t)
	say(t, "key invariant: a.go and c.go are NOT held by "+alice.handle+" either.")
	say(t, "the whole batch aborted. no chmod side effects, no partial state.")
	beat(t)
	_, out = alice.do(t, "status", "--mine")
	mustContain(t, out, "no locks")
}

// TestDemo_18_ReadsAreFree — NS invariant #6: loto coordinates writes only.
//
// A locked file is stripped to 0444. Peers can still read it — they just
// can't write. This is what makes "look before you leap" cheap.
func TestDemo_18_ReadsAreFree(t *testing.T) {
	head(t, 18, "reads are free — locked file stays readable at 0444")
	repo := withTempProject(t)
	rel := "shared.go"
	full := filepath.Join(repo, rel)
	if err := os.WriteFile(full, []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	alice, bob := cast(t)

	say(t, alice.handle+" locks shared.go.")
	beat(t)
	alice.do(t, "lock", rel, "--intent", "edit in progress")
	beat(t)

	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	mode := st.Mode().Perm()
	t.Logf("    %-10s   mode: %#o (owner-write stripped)", "fs", mode)
	if mode&0o200 != 0 {
		t.Fatalf("expected owner-write stripped, got %#o", mode)
	}
	beat(t)

	say(t, bob.handle+" — without a lock — reads the file. should just work.")
	beat(t)
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("read while locked failed: %v", err)
	}
	t.Logf("    %-10s   read %d bytes: %q", bob.handle, len(data), strings.TrimSpace(string(data)))
	beat(t)
	say(t, "now bob tries to WRITE without a lock. filesystem refuses (mode 0444).")
	beat(t)
	if err := os.WriteFile(full, []byte("clobber\n"), 0o644); err == nil {
		t.Fatalf("expected write to fail while locked")
	} else {
		t.Logf("    %-10s   write blocked: %v", bob.handle, err)
	}
	beat(t)
	say(t, "loto coordinates writes only. reads stay free — review, grep, snipe, all safe.")
}

// TestDemo_19_ForceBreak — NS invariant #8: no silent dispossession.
//
// A peer can break another agent's lock with `unlock --force -t "why"`.
// The break is auditable: tag travels with the release, write mode is
// restored, and the new owner can claim cleanly.
func TestDemo_19_ForceBreak(t *testing.T) {
	head(t, 19, "force-break — taking over with an audit trail")
	repo := withTempProject(t)
	touch(t, repo, "stuck.go")
	alice, bob := cast(t)

	say(t, alice.handle+" claims stuck.go and then disappears (AFK, crash, whatever).")
	beat(t)
	alice.do(t, "lock", "stuck.go", "--intent", "long migration")
	beat(t)
	say(t, bob.handle+" needs the file. lock would be blocked. --force breaks it,")
	say(t, "and the -t tag explains why — the audit trail is the whole point.")
	beat(t)
	bob.do(t, "unlock", "stuck.go", "--force", "-t", "alice afk 30m, taking over for hotfix")
	beat(t)
	say(t, "filesystem mode restored — file is writable again.")
	full := filepath.Join(repo, "stuck.go")
	st, err := os.Stat(full)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o200 == 0 {
		t.Fatalf("expected owner-write restored after force-break, got %#o", st.Mode().Perm())
	}
	t.Logf("    %-10s   mode: %#o", "fs", st.Mode().Perm())
	beat(t)
	say(t, bob.handle+" can now claim. no silent dispossession — the break left a tag.")
	beat(t)
	_, out := bob.do(t, "lock", "stuck.go", "--intent", "hotfix")
	mustContain(t, out, "✓ locked")
}

// TestDemo_20_LazyGCOnAcquire — NS reclamation layer #1.
//
// `loto doctor` is the manual cleanup path. The passive path is lazy GC:
// every `loto lock` sweeps expired rows before evaluating its own request.
// No daemon, no scheduler — the next acquirer pays the cost.
func TestDemo_20_LazyGCOnAcquire(t *testing.T) {
	head(t, 20, "lazy GC — next acquire cleans expired rows automatically")
	repo := withTempProject(t)
	touch(t, repo, "abandoned.go")
	alice, bob := cast(t)

	say(t, alice.handle+" locks with a 1ms TTL. think: short-claim that timed out.")
	beat(t)
	alice.do(t, "lock", "abandoned.go", "--ttl", "1ms", "--intent", "quick touch")
	time.Sleep(50 * time.Millisecond)
	beat(t)
	say(t, "TTL has expired. no doctor run, no sweep, no daemon.")
	say(t, bob.handle+" just tries to lock — and lazy GC reaps alice's row first.")
	beat(t)
	_, out := bob.do(t, "lock", "abandoned.go", "--intent", "follow-up")
	mustContain(t, out, "✓ locked")
	beat(t)
	say(t, "this is reclamation layer #1: every acquire pays a small GC tax")
	say(t, "so the system stays honest without a background process.")
	beat(t)
	_, out = bob.do(t, "status", "abandoned.go")
	mustContain(t, out, `intent="follow-up"`)
}
