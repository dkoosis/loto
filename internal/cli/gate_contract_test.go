package cli

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// pinContractSession gives each test its own session id and its own temp dir,
// so the once-per-session marker one test writes can never silence another.
func pinContractSession(t *testing.T, sid string) {
	t.Helper()
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("LOTO_SESSION_ID", sid)
}

func TestWarnIfContractStale_NewerHookWarns(t *testing.T) {
	pinContractSession(t, "sess-newer")
	t.Setenv(gateContractEnv, strconv.Itoa(GateContractVersion+1))

	var buf bytes.Buffer
	if !warnIfContractStale(&buf) {
		t.Fatal("a hook needing a newer contract must warn")
	}
	got := buf.String()
	if !strings.Contains(got, "⚠ gate=contract-stale") {
		t.Errorf("missing the ⚠ triage row: %q", got)
	}
	if !strings.Contains(got, "binary="+strconv.Itoa(GateContractVersion)) ||
		!strings.Contains(got, "hook="+strconv.Itoa(GateContractVersion+1)) {
		t.Errorf("row must name both versions so the reader can tell which side is stale: %q", got)
	}
	if !strings.Contains(got, "```bash") {
		t.Errorf("an actionable finding owes a fix block (.claude/rules/design.md): %q", got)
	}
}

// The healthy path must cost nothing. A gate that prints on every tool call
// teaches the fleet to filter ⚠ rows, which is how the next real one is missed.
func TestWarnIfContractStale_MatchingAndOlderAreSilent(t *testing.T) {
	for _, want := range []int{GateContractVersion, GateContractVersion - 1} {
		t.Run("contract="+strconv.Itoa(want), func(t *testing.T) {
			pinContractSession(t, "sess-quiet-"+strconv.Itoa(want))
			t.Setenv(gateContractEnv, strconv.Itoa(want))
			var buf bytes.Buffer
			if warnIfContractStale(&buf) || buf.Len() != 0 {
				t.Errorf("contract %d must be silent, got %q", want, buf.String())
			}
		})
	}
}

// A hook that garbles the variable must not make loto noisier than one that
// omits it — the value is machine-written, and a parse failure says nothing
// about whether the binary is stale.
func TestWarnIfContractStale_UnsetOrGarbledIsSilent(t *testing.T) {
	for _, raw := range []string{"", "  ", "abc", "-3", "1.5"} {
		t.Run("value="+strconv.Quote(raw), func(t *testing.T) {
			pinContractSession(t, "sess-garbled")
			t.Setenv(gateContractEnv, raw)
			var buf bytes.Buffer
			if warnIfContractStale(&buf) || buf.Len() != 0 {
				t.Errorf("value %q must be silent, got %q", raw, buf.String())
			}
		})
	}
}

func TestWarnIfContractStale_OncePerSession(t *testing.T) {
	pinContractSession(t, "sess-repeat")
	t.Setenv(gateContractEnv, strconv.Itoa(GateContractVersion+1))

	var first, second bytes.Buffer
	if !warnIfContractStale(&first) {
		t.Fatal("first call must warn")
	}
	if warnIfContractStale(&second) {
		t.Errorf("second call in one session must stay quiet, got %q", second.String())
	}
}

// A hook upgraded mid-session is a new fact, not a repeat of the old one.
func TestWarnIfContractStale_NewerHookWarnsAgain(t *testing.T) {
	pinContractSession(t, "sess-upgrade")

	t.Setenv(gateContractEnv, strconv.Itoa(GateContractVersion+1))
	var first bytes.Buffer
	if !warnIfContractStale(&first) {
		t.Fatal("first call must warn")
	}
	t.Setenv(gateContractEnv, strconv.Itoa(GateContractVersion+2))
	var second bytes.Buffer
	if !warnIfContractStale(&second) {
		t.Errorf("a further-ahead hook must warn again, got %q", second.String())
	}
}

// Without a pinned session there is nothing to dedupe against. Warning every
// time is the deliberate choice: one shared marker would silence every session
// after the first, which is the silent-rot failure this bead exists to fix.
func TestWarnIfContractStale_UnpinnedSessionWarnsEveryTime(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("LOTO_SESSION_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "")
	t.Setenv(gateContractEnv, strconv.Itoa(GateContractVersion+1))

	for i := range 2 {
		var buf bytes.Buffer
		if !warnIfContractStale(&buf) {
			t.Fatalf("call %d must warn when no session pins a marker", i)
		}
	}
}

func TestSanitizeMarker_KeepsOnePathComponent(t *testing.T) {
	cases := map[string]string{
		"abc-123_XY": "abc-123_XY",
		"../../etc":  "______etc",
		"a/b":        "a_b",
		"sp ace":     "sp_ace",
		`back\slash`: "back_slash",
		"unicode-ü":  "unicode-_",
		"":           "",
	}
	for in, want := range cases {
		if got := sanitizeMarker(in); got != want {
			t.Errorf("sanitizeMarker(%q) = %q, want %q", in, got, want)
		}
	}
}
