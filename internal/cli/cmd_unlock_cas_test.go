package cli

import (
	"bytes"
	"io"
	"regexp"
	"strings"
	"testing"
)

const tcFlagExpectHolder = "--expect-holder"

// holdTokenRe pulls the `owner@epoch` --expect-holder token out of a status
// row. Reading it back out of real CLI output is the point: if the two halves
// a caller needs aren't both on the line loto prints, the flag is unusable and
// this test fails at the parse, not at the assertion.
var holdTokenRe = regexp.MustCompile(`owner=(\S+) epoch=(\d+)`)

func holdTokenFor(t *testing.T, target string) string {
	t.Helper()
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, target}, &out, io.Discard); code != 0 {
		t.Fatalf("status exit %d: %s", code, out.String())
	}
	m := holdTokenRe.FindStringSubmatch(out.String())
	if m == nil {
		t.Fatalf("status must print owner= and epoch= so a caller can build the --expect-holder token; got %q", out.String())
	}
	return m[1] + "@" + m[2]
}

// TestUnlockForce_ExpectHolder_RefusesAfterReacquireByThirdAgent walks the
// bead's scenario through the CLI exactly as an agent would: read `loto
// status`, decide to break, and have the ground move first. The break must
// exit non-zero, leave carol's lock alone, and say what it found.
func TestUnlockForce_ExpectHolder_RefusesAfterReacquireByThirdAgent(t *testing.T) {
	withTempProject(t)
	alice, bob, carol := threeAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("alice lock exit %d", code)
	}

	// bob reads the holder he intends to break.
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	token := holdTokenFor(t, tcTargetA)

	// The window: alice lets go, carol takes the path.
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdUnlock, tcTargetA}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("alice unlock exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", carol.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentWrite}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("carol lock exit %d", code)
	}

	// bob's break lands on a hold he never read.
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", "taking over", tcFlagExpectHolder, token}, &out, &errBuf)
	if code == 0 {
		t.Fatalf("stale --expect-holder must exit non-zero; stdout=%q stderr=%q", out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "holder-changed") {
		t.Errorf("refusal must name the reason token; stderr=%q", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "actual="+carol.UUID+"@") {
		t.Errorf("refusal must name the current holder so bob can re-decide; stderr=%q", errBuf.String())
	}
	if strings.Contains(out.String(), "✓ broken") {
		t.Errorf("a refused break must not report success; stdout=%q", out.String())
	}

	// carol still holds the path.
	var status bytes.Buffer
	Run([]string{tcCmdStatus, tcTargetA}, &status, io.Discard)
	if !strings.Contains(status.String(), "owner="+carol.UUID) {
		t.Errorf("carol must keep her lock; status=%q", status.String())
	}
}

// TestUnlockForce_ExpectHolder_MatchingBreaks: nothing moved, so the break
// lands. Guards against a compare-and-swap that refuses everything.
func TestUnlockForce_ExpectHolder_MatchingBreaks(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("alice lock exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	token := holdTokenFor(t, tcTargetA)

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", "deadline", tcFlagExpectHolder, token}, &out, &errBuf); code != 0 {
		t.Fatalf("matching --expect-holder must succeed, exit %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ broken") {
		t.Errorf("want a broken row; stdout=%q", out.String())
	}
}

// TestUnlockForce_ExpectHolder_MalformedToken: a token that is not
// `owner@epoch` is a usage error, refused before the store is opened. Silently
// treating it as "no expectation" would be the bug shipped back.
func TestUnlockForce_ExpectHolder_MalformedToken(t *testing.T) {
	withTempProject(t)
	alice := pinAgent(t)
	_ = alice
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock exit %d", code)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", tcIntentWhy, tcFlagExpectHolder, "alice"}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("malformed token must be a usage error (exit 2), got %d; stdout=%q stderr=%q",
			code, out.String(), errBuf.String())
	}
	var status bytes.Buffer
	Run([]string{tcCmdStatus, tcTargetA}, &status, io.Discard)
	if strings.Contains(status.String(), "✓ free") {
		t.Errorf("a rejected invocation must not have broken anything; status=%q", status.String())
	}
}

// TestUnlockForce_ExpectHolder_MultiTargetRefused: --expect-holder names a
// hold, not a path, so it cannot say which of several targets it belongs to.
// Refuse rather than bind it to all of them, which would exempt the rest.
func TestUnlockForce_ExpectHolder_MultiTargetRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcStoreStoreGo, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock exit %d", code)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcStoreStoreGo, tcFlagForce, "-t", tcIntentWhy, tcFlagExpectHolder, tcHoldAlice1}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("multi-target --expect-holder must exit 2, got %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "exactly one target") {
		t.Errorf("diagnostic must say why; stderr=%q", errBuf.String())
	}
}

// TestUnlockForce_ExpectHolder_WithoutForceRefused: on a plain unlock the flag
// has nothing to compare — release only ever touches the caller's own row —
// so accepting it would let a caller believe it had a guarantee it does not.
func TestUnlockForce_ExpectHolder_WithoutForceRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock exit %d", code)
	}
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagExpectHolder, tcHoldAlice1}, &out, &errBuf); code != 2 {
		t.Fatalf("--expect-holder without --force must exit 2, got %d; stderr=%q", code, errBuf.String())
	}
}

// TestUnlockForce_BareForceStillBreaksBlind: the deliberate blind break, kept
// working and documented in the help. A sweep over a dead peer's territory has
// no generation to name.
func TestUnlockForce_BareForceStillBreaksBlind(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("alice lock exit %d", code)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", "sweep"}, &out, &errBuf); code != 0 {
		t.Fatalf("bare --force must still break, exit %d; stderr=%q", code, errBuf.String())
	}
	if !strings.Contains(out.String(), "✓ broken") {
		t.Errorf("want a broken row; stdout=%q", out.String())
	}
}

// TestUnlockForce_ExpectHolder_DuplicateTokenRefused: repeating a token
// misstates the holder set. Deduping it would let a typo pass a check whose
// only job is exactness.
func TestUnlockForce_ExpectHolder_DuplicateTokenRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock exit %d", code)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcTargetA, tcFlagForce, "-t", tcIntentWhy,
		tcFlagExpectHolder, tcHoldAlice1, tcFlagExpectHolder, tcHoldAlice1}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("duplicate --expect-holder must exit 2, got %d; stderr=%q", code, errBuf.String())
	}
}

// TestUnlockForce_ExpectHolder_WithAllRefused: --all takes the
// ReleaseBySession path, which reads neither the positional target nor the
// expectation. Accepting the combination would sweep every lock the caller
// owns while the invocation reads as a guarded break of one path — the silent
// dispossession this whole flag exists to stop, aimed at the caller instead.
func TestUnlockForce_ExpectHolder_WithAllRefused(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	if code := Run([]string{tcCmdLock, tcTargetA, tcStoreStoreGo, "-t", tcIntentTest}, io.Discard, io.Discard); code != 0 {
		t.Fatalf("lock exit %d", code)
	}
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdUnlock, tcFlagAll, tcTargetA, tcFlagForce, "-t", tcIntentWhy,
		tcFlagExpectHolder, tcHoldAlice1}, &out, &errBuf)
	if code != 2 {
		t.Fatalf("--all with --expect-holder must exit 2, got %d; stdout=%q stderr=%q",
			code, out.String(), errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "--all") {
		t.Errorf("diagnostic must name the conflicting flag; stderr=%q", errBuf.String())
	}
	// The refusal has to happen before the sweep, not after it.
	var status bytes.Buffer
	Run([]string{tcCmdStatus, tcTargetA}, &status, io.Discard)
	if strings.Contains(status.String(), "✓ free") {
		t.Errorf("a refused invocation must not have released anything; status=%q", status.String())
	}
	status.Reset()
	Run([]string{tcCmdStatus, tcStoreStoreGo}, &status, io.Discard)
	if strings.Contains(status.String(), "✓ free") {
		t.Errorf("the sibling lock must survive too; status=%q", status.String())
	}
}
