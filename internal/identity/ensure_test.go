package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tcSessionA = "aaaaaaaa-1111-4111-8111-111111111111"
	tcSessionB = "bbbbbbbb-2222-4222-8222-222222222222"
	tcPinnedID = "cccccccc-3333-4333-8333-333333333333"
	tcStamp    = "sibling-1" // a LOTO_SUBAGENT_ID stamp
)

// clearIdentityEnv strips every variable Ensure reads so a case starts from
// "nothing pins" and adds back only what it is about.
func clearIdentityEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"LOTO_AGENT_ID", "CLAUDE_CODE_SESSION_ID", "LOTO_SUBAGENT_ID", "LOTO_SESSION_ID"} {
		unsetEnv(t, k)
	}
	t.Setenv("HOME", t.TempDir())
	unsetEnv(t, "LOTO_BASE")
}

// unsetEnv is the Unsetenv analogue of t.Setenv: LOTO_AGENT_ID's contract
// turns on set-vs-unset, which t.Setenv cannot express.
func unsetEnv(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	t.Cleanup(func() {
		if had {
			os.Setenv(key, prev) //nolint:usetesting // restore in a Cleanup closure; t.Setenv is unusable here and can't express the unset case this helper exists for
		} else {
			os.Unsetenv(key)
		}
	})
	os.Unsetenv(key)
}

// TestEnsureOwnerIsTheSessionID is the loto-jnid contract: inside a Claude
// Code session the owner IS CLAUDE_CODE_SESSION_ID, verbatim, and nothing is
// written to disk to get there.
func TestEnsureOwnerIsTheSessionID(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != tcSessionA {
		t.Fatalf("owner = %q, want the session id %q", a.UUID, tcSessionA)
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".loto")); len(entries) != 0 {
		t.Errorf("Ensure must not touch disk; found %d entries under ~/.loto", len(entries))
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionB)
	b, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if b.UUID != tcSessionB || b.UUID == a.UUID {
		t.Errorf("two sessions must be two owners: %q vs %q", a.UUID, b.UUID)
	}
}

// TestEnsureUnpinnedRefuses: nothing in the env → ErrUnpinned, never a minted
// persistent agent (the old resolution step 4 is gone). Ephemeral is the
// display-only substitute and owns a fresh id each call.
func TestEnsureUnpinnedRefuses(t *testing.T) {
	clearIdentityEnv(t)

	if _, err := Ensure(context.Background()); !errors.Is(err, ErrUnpinned) {
		t.Fatalf("unpinned Ensure err = %v, want ErrUnpinned", err)
	}
	for _, want := range []string{"CLAUDE_CODE_SESSION_ID", "LOTO_AGENT_ID"} {
		if !strings.Contains(ErrUnpinned.Error(), want) {
			t.Errorf("ErrUnpinned must name %q: %q", want, ErrUnpinned)
		}
	}
	if !PinnedByEnv() {
		return
	}
	t.Error("PinnedByEnv() = true with nothing set")
}

func TestEphemeralIsThrowaway(t *testing.T) {
	clearIdentityEnv(t)
	a, b := Ephemeral(), Ephemeral()
	if a.UUID == "" || a.UUID == b.UUID {
		t.Errorf("Ephemeral must mint a fresh id each call: %q vs %q", a.UUID, b.UUID)
	}
	if !agentIDShape.MatchString(a.UUID) {
		t.Errorf("Ephemeral id %q is not uuid-shaped", a.UUID)
	}
}

// TestEnsureAgentIDPinsWithoutRecord: LOTO_AGENT_ID is an explicit pin. There
// is no registry to resolve it against, so the value is the owner — the old
// "stale LOTO_AGENT_ID" hard error (no record on disk) is gone with the disk.
func TestEnsureAgentIDPinsWithoutRecord(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_AGENT_ID", tcPinnedID)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA) // the pin wins over the session

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != tcPinnedID {
		t.Errorf("owner = %q, want the pin %q", a.UUID, tcPinnedID)
	}
}

func TestEnsureRejectsMalformedAgentID(t *testing.T) {
	clearIdentityEnv(t)
	for _, bad := range []string{"not-a-uuid", "../escape", "AAAAAAAA-1111-4111-8111-111111111111"} {
		t.Setenv("LOTO_AGENT_ID", bad)
		if _, err := Ensure(context.Background()); !errors.Is(err, errInvalidAgentID) {
			t.Errorf("LOTO_AGENT_ID=%q: err = %v, want errInvalidAgentID", bad, err)
		}
	}
}

// TestEnsureBlankAgentIDIsEphemeral: set-but-empty LOTO_AGENT_ID is the fleet
// dispatcher's explicit throwaway, and a present session id must NOT rescue
// it (loto-s3l).
func TestEnsureBlankAgentIDIsEphemeral(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == tcSessionA || !agentIDShape.MatchString(a.UUID) {
		t.Errorf("blank LOTO_AGENT_ID must mint a throwaway, got %q", a.UUID)
	}
	if PinnedByEnv() {
		t.Error("blank LOTO_AGENT_ID must read as unpinned")
	}
}

// TestEnsureRejectsTraversalSessionID: the session id becomes a file name
// under ~/.loto/session, so a traversal-shaped value is refused before any
// path is built.
func TestEnsureRejectsTraversalSessionID(t *testing.T) {
	clearIdentityEnv(t)
	for _, bad := range []string{"../escape", "a/b", `a\b`} {
		t.Setenv("CLAUDE_CODE_SESSION_ID", bad)
		if _, err := Ensure(context.Background()); !errors.Is(err, errInvalidSessionID) {
			t.Errorf("CLAUDE_CODE_SESSION_ID=%q: err = %v, want errInvalidSessionID", bad, err)
		}
	}
}

// TestEnsureSubagentIDDerivesDistinctOwners: two siblings sharing one session
// get two owners, each stable across calls, neither equal to the session's
// own owner, with nothing written to disk (loto-fs84, loto-jnid).
func TestEnsureSubagentIDDerivesDistinctOwners(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)

	t.Setenv("LOTO_SUBAGENT_ID", tcStamp)
	one, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Ensure(context.Background())
	t.Setenv("LOTO_SUBAGENT_ID", "sibling-2")
	two, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if one.UUID != again.UUID {
		t.Errorf("sibling-1 resolved %q then %q; must be stable", one.UUID, again.UUID)
	}
	if one.UUID == two.UUID {
		t.Errorf("siblings collapsed onto one owner %q", one.UUID)
	}
	if one.UUID == tcSessionA || two.UUID == tcSessionA {
		t.Error("a stamped sibling must not be the session's own owner")
	}
	if !agentIDShape.MatchString(one.UUID) {
		t.Errorf("derived owner %q is not uuid-shaped", one.UUID)
	}
	if !PinnedByEnv() {
		t.Error("a stamped sibling with a session id must read as pinned")
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".loto")); len(entries) != 0 {
		t.Errorf("derivation must not touch disk; found %d entries", len(entries))
	}
}

// TestEnsureSubagentIDPrecedesAgentID: a stamped sibling diverges from the
// LOTO_AGENT_ID it inherits from its parent — that is the whole point of the
// stamp.
func TestEnsureSubagentIDPrecedesAgentID(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	t.Setenv("LOTO_AGENT_ID", tcPinnedID)
	t.Setenv("LOTO_SUBAGENT_ID", tcStamp)

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == tcPinnedID {
		t.Error("stamped sibling resolved to the inherited LOTO_AGENT_ID; siblings would collapse")
	}
}

// TestEnsureSubagentIDFailsOpen: a stamp with no session id to derive from, or
// a traversal-shaped stamp, is ignored rather than erroring — the stamp is
// never load-bearing.
func TestEnsureSubagentIDFailsOpen(t *testing.T) {
	clearIdentityEnv(t)

	t.Setenv("LOTO_SUBAGENT_ID", tcStamp)
	if _, err := Ensure(context.Background()); !errors.Is(err, ErrUnpinned) {
		t.Errorf("stamp without a session id: err = %v, want ErrUnpinned (fell open to unbound)", err)
	}
	if SubagentIDPins(tcStamp) {
		t.Error("SubagentIDPins must be false with no session id")
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	t.Setenv("LOTO_SUBAGENT_ID", "../escape")
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != tcSessionA {
		t.Errorf("malformed stamp must fall open to the session owner, got %q", a.UUID)
	}
}

// TestEnsureParentResolvesHiddenIdentity: under a stamp, EnsureParent hands
// back the owner the process would be without it — the id the sibling's own
// unstamped `loto lock` rows carry (loto-wofb).
func TestEnsureParentResolvesHiddenIdentity(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)

	if _, ok, err := EnsureParent(context.Background()); ok || err != nil {
		t.Fatalf("no stamp: handled=%v err=%v, want false/nil", ok, err)
	}

	t.Setenv("LOTO_SUBAGENT_ID", tcStamp)
	parent, ok, err := EnsureParent(context.Background())
	if err != nil || !ok {
		t.Fatalf("stamped: handled=%v err=%v", ok, err)
	}
	if parent.UUID != tcSessionA {
		t.Errorf("parent = %q, want the session owner %q", parent.UUID, tcSessionA)
	}
	self, _ := Ensure(context.Background())
	if self.UUID == parent.UUID {
		t.Error("EnsureParent returned the sibling's own derived owner, not the hidden one")
	}
}

func TestEnsureRespectsCtxCancel(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Ensure(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Ensure under cancelled ctx: err = %v, want context.Canceled", err)
	}
}

func TestDeriveUUIDIsStableAndShaped(t *testing.T) {
	a := deriveUUID(tcSessionA, "x")
	if a != deriveUUID(tcSessionA, "x") {
		t.Error("deriveUUID not deterministic")
	}
	if a == deriveUUID(tcSessionA, "y") || a == deriveUUID(tcSessionB, "x") {
		t.Error("deriveUUID collides across distinct inputs")
	}
	if !agentIDShape.MatchString(a) || a[14] != '4' {
		t.Errorf("deriveUUID %q is not v4-shaped", a)
	}
}

func TestSessionIDFromEnvPrecedence(t *testing.T) {
	clearIdentityEnv(t)
	if got := SessionIDFromEnv(); got != "" {
		t.Errorf("nothing set: %q, want empty", got)
	}
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	if got := SessionIDFromEnv(); got != tcSessionA {
		t.Errorf("claude session: %q, want %q", got, tcSessionA)
	}
	t.Setenv("LOTO_SESSION_ID", "override")
	if got := SessionIDFromEnv(); got != "override" {
		t.Errorf("LOTO_SESSION_ID must win: %q", got)
	}
}
