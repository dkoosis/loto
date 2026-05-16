package cli

// Demo tests — animated, play-shaped walk-throughs of every loto primitive.
//
// Run with:
//
//	go test -v -run Demo ./internal/cli
//
// Each test is a tiny play. Narration sits to the left of the screen; commands
// are prefixed with the actor's handle and a `❯` prompt; output shows only the
// key line. Read top-to-bottom, you watch two agents (alice and bob) negotiate
// territory through loto.

import (
	"bytes"
	"fmt"
	"io"
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

// ─── demos ───────────────────────────────────────────────────────────────────

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
	if err := os.WriteFile(filepath.Join(repo, "b.go"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
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
	beat(t)
	say(t, "exit 1 + blocker= line says: do NOT edit. the report names "+alice.handle+".")
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
	if code != 1 {
		t.Fatalf("expected blocked, got exit %d", code)
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
	say(t, "she's done. she tags the unlock so the history is honest.")
	beat(t)
	_, out := alice.do(t, "unlock", "internal/store/store.go", "-t", "done")
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
	say(t, "no --mine = every active lock in the repo, with handles.")
}

func TestDemo_07_DoctorStaleLock(t *testing.T) {
	head(t, 7, "doctor — find and repair stale locks")
	withTempProject(t)
	a := solo(t)

	say(t, "a session crashed mid-edit and left a lock behind.")
	say(t, "we fake that by locking with a 1ms TTL.")
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
	say(t, "include this verbatim in any bug report.")
	beat(t)
	a.do(t, "version")
}

// TestDemo_00_Index prints a one-screen table of contents. The 00 prefix sorts
// it ahead of the others under `go test -v`.
func TestDemo_00_Index(t *testing.T) {
	rows := []string{
		"01  whoami           identity of the current session",
		"02  lock + status    claim a file, inspect what you hold",
		"03  check            pre-flight before editing",
		"04  contention       two agents racing for the same file",
		"05  handoff          unlock so a peer can claim",
		"06  status (global)  every active lock, across agents",
		"07  doctor           find and repair stale locks",
		"08  version          meta",
	}
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString("    ┌─ loto demo index ")
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
	_ = io.Discard
}
