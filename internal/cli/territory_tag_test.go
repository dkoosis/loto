package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ttNoteText   = "loto-abc: rebase before you touch claims.go"
	ttDirPrefix  = "internal/store"
	ttInsidePath = "internal/store/store.go"
)

// mkTerritory creates the dir + file the prefix tests need. withTempProject
// only seeds top-level a.go/b.go/c.go.
func mkTerritory(t *testing.T, repo string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repo, ttDirPrefix), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ttInsidePath), []byte("package store\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runOK runs argv, fails the test on a non-zero exit, and returns stdout.
func runOK(t *testing.T, argv ...string) string {
	t.Helper()
	var out, errBuf bytes.Buffer
	if code := Run(argv, &out, &errBuf); code != 0 {
		t.Fatalf("%v exit=%d out=%q err=%q", argv, code, out.String(), errBuf.String())
	}
	return out.String()
}

// tagID pulls the `id=…` value out of a tag/territory-tag confirmation row.
func tagID(t *testing.T, out string) string {
	t.Helper()
	for f := range strings.FieldsSeq(out) {
		if v, ok := strings.CutPrefix(f, "id="); ok {
			return v
		}
	}
	t.Fatalf("no id= in %q", out)
	return ""
}

// TestTagLockedPathUnchanged is the no-regression half of the auto-route: a
// held target must take today's path, byte for byte. The routing decision is
// made by the ground, so this is the assertion that the ground is read right.
func TestTagLockedPathUnchanged(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	runOK(t, tcCmdLock, tcTargetA, "-t", tcIntentTest)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	out := runOK(t, tcCmdTag, tcTargetA, "loto-abc:", "want", "next")
	if !strings.HasPrefix(out, "✓ tag id=t-") {
		t.Fatalf("a held target must still take a held tag, got %q", out)
	}
	if strings.Contains(out, "territory-tag") {
		t.Errorf("held target must not produce a territory tag: %q", out)
	}
}

func TestTagDirectoryPrefix(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	mkTerritory(t, repo)

	for _, arg := range []string{ttDirPrefix, ttDirPrefix + "/"} {
		out := runOK(t, tcCmdTag, arg, ttNoteText)
		if !strings.Contains(out, "prefix="+ttDirPrefix+" ") {
			t.Errorf("%q must land on prefix %q, got %q", arg, ttDirPrefix, out)
		}
	}
}

// A glob is the right idea in the wrong spelling — loto's territory vocabulary
// is the bare prefix — so the rejection owes a fix block, not just a refusal.
func TestTagGlobRejectedWithFix(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	mkTerritory(t, repo)

	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdTag, ttDirPrefix + "/**", ttNoteText}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2 for a glob, got %d (%q)", code, errBuf.String())
	}
	got := errBuf.String()
	if !strings.Contains(got, "```bash") || !strings.Contains(got, "loto tag "+ttDirPrefix) {
		t.Errorf("glob rejection must show the bare-prefix spelling: %q", got)
	}
}

// A silently-ignored flag is worse than a refusal: --ttl means nothing on a
// held target, whose note lives exactly as long as the lock does.
func TestTagTTLFlagRejectedOnLockedTarget(t *testing.T) {
	withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	runOK(t, tcCmdLock, tcTargetA, "-t", tcIntentTest)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdTag, tcTargetA, tcFlagTTL, "1h", "loto-abc:", "note"}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2, got %d (%q)", code, out.String())
	}
	if !strings.Contains(errBuf.String(), "--ttl applies to territory tags") {
		t.Errorf("refusal must name the reason: %q", errBuf.String())
	}
	// Nothing may be written on the refusal path.
	if s := runOK(t, tcCmdStatus); strings.Contains(s, "territory-tag") {
		t.Errorf("a refused --ttl must write nothing: %q", s)
	}
}

// TestLockSurfacesCoveringTerritoryTag is the bead's acceptance criterion: bob
// pins a note on ground nobody holds, and alice — who has never heard of bob —
// learns about it by walking onto that ground.
func TestLockSurfacesCoveringTerritoryTag(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	out := runOK(t, tcCmdLock, ttInsidePath, "-t", tcIntentTest)
	if !strings.Contains(out, "✓ locked count=1") {
		t.Fatalf("lock must still succeed and print its own rows first: %q", out)
	}
	if !strings.Contains(out, "ℹ territory-tag id=tt-") || !strings.Contains(out, ttNoteText) {
		t.Fatalf("a note covering the locked path must surface with its text: %q", out)
	}
}

func TestClaimSurfacesCoveringTerritoryTag(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	out := runOK(t, "claim", ttDirPrefix, "-t", tcIntentTest)
	if !strings.Contains(out, "ℹ territory-tag id=tt-") {
		t.Fatalf("claiming the ground must surface what is pinned to it: %q", out)
	}
}

// The no-conflict early return is the common case and never reaches the bottom
// of cmdCheck — a note that only surfaced on the conflict path would miss
// nearly every check (the loto-qoq trap, re-set here).
func TestCheckSurfacesCoveringTerritoryTagOnCleanPath(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	out := runOK(t, tcCmdCheck, ttInsidePath)
	if !strings.Contains(out, "✓ no conflicts") {
		t.Fatalf("precondition: nothing is locked here, got %q", out)
	}
	if !strings.Contains(out, "ℹ territory-tag id=tt-") {
		t.Fatalf("check must surface the note on the clean path too: %q", out)
	}
}

// TestCheckGateOutputUnchangedByTerritoryTag is D6 as an assertion. `--gate` is
// the pinned machine surface trixi's PreToolUse hook consumes, and it just went
// 24× faster (#235). A note never denies, so it has no verdict to contribute —
// and the agent that proceeds past the gate then runs `loto lock`, where the
// note surfaces before a single byte is edited.
func TestCheckGateOutputUnchangedByTerritoryTag(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var before bytes.Buffer
	codeBefore := Run([]string{tcCmdCheck, tcFlagGate, ttInsidePath}, &before, io.Discard)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var after bytes.Buffer
	codeAfter := Run([]string{tcCmdCheck, tcFlagGate, ttInsidePath}, &after, io.Discard)

	if codeBefore != codeAfter {
		t.Errorf("a note must not change the gate's verdict: %d then %d", codeBefore, codeAfter)
	}
	if before.String() != after.String() {
		t.Errorf("gate stdout must be byte-identical:\n before %q\n after  %q", before.String(), after.String())
	}
}

func TestStatusListsLiveTerritoryTags(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	mkTerritory(t, repo)

	if out := runOK(t, tcCmdStatus); strings.Contains(out, "territory-tag") {
		t.Fatalf("the section must be absent with no notes: %q", out)
	}
	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)
	out := runOK(t, tcCmdStatus)
	if !strings.Contains(out, "ℹ territory-tags count=1") || !strings.Contains(out, ttNoteText) {
		t.Fatalf("status must list what is pinned to this repo: %q", out)
	}
}

// The footer answers "what should I know about ground I just took", and being
// told your own note is noise. status answers "what is pinned here", where your
// own note is part of the answer. Same split ListAliveForOwner already makes.
func TestTerritoryTagNotSurfacedToItsAuthor(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	mkTerritory(t, repo)

	runOK(t, tcCmdTag, ttDirPrefix, ttNoteText)
	lockOut := runOK(t, tcCmdLock, ttInsidePath, "-t", tcIntentTest)
	if strings.Contains(lockOut, "ℹ territory-tag") {
		t.Errorf("the footer must not echo the caller's own note: %q", lockOut)
	}
	if statusOut := runOK(t, tcCmdStatus); !strings.Contains(statusOut, "ℹ territory-tag") {
		t.Errorf("status must still show it: %q", statusOut)
	}
}

// TestExpiredTerritoryTagNotSurfacedButDoctorReports walks D2 end to end. A
// note that vanishes with nobody having read it is the mail-lost failure
// returning, so expiry means "stop surfacing, report, then sweep" — never
// "disappear quietly".
func TestExpiredTerritoryTagNotSurfacedButDoctorReports(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	runOK(t, tcCmdTag, ttDirPrefix, tcFlagTTL, "1ns", ttNoteText)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if out := runOK(t, tcCmdLock, ttInsidePath, "-t", tcIntentTest); strings.Contains(out, "ℹ territory-tag") {
		t.Errorf("an expired note must not surface on lock: %q", out)
	}
	if out := runOK(t, tcCmdCheck, ttInsidePath); strings.Contains(out, "ℹ territory-tag") {
		t.Errorf("an expired note must not surface on check: %q", out)
	}
	if out := runOK(t, tcCmdStatus); strings.Contains(out, "ℹ territory-tag") {
		t.Errorf("an expired note must not surface on status: %q", out)
	}

	var doctorOut, doctorErr bytes.Buffer
	Run([]string{tcCmdDoctor}, &doctorOut, &doctorErr)
	got := doctorOut.String()
	if !strings.Contains(got, "⚠ expired_territory_tag id=tt-") || !strings.Contains(got, ttNoteText) {
		t.Fatalf("doctor must report the note nobody read, with its text: %q", got)
	}
	if !strings.Contains(got, "expired_territory_tags=1") {
		t.Errorf("the triage line must carry the count: %q", got)
	}

	Run([]string{tcCmdDoctor, tcFlagRepair}, io.Discard, io.Discard)
	var afterOut bytes.Buffer
	Run([]string{tcCmdDoctor}, &afterOut, io.Discard)
	if strings.Contains(afterOut.String(), "expired_territory_tag id=") {
		t.Errorf("--repair must sweep it: %q", afterOut.String())
	}
}

// One verb, one vocabulary, and the two tables must stay unmixed.
func TestAckRoutesByIDPrefix(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	runOK(t, tcCmdLock, tcTargetA, "-t", tcIntentTest)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	heldID := tagID(t, runOK(t, tcCmdTag, tcTargetA, "loto-abc:", "want", "next"))
	territoryID := tagID(t, runOK(t, tcCmdTag, ttDirPrefix, ttNoteText))
	if !strings.HasPrefix(heldID, "t-") || !strings.HasPrefix(territoryID, "tt-") {
		t.Fatalf("id prefixes must differ: held=%q territory=%q", heldID, territoryID)
	}

	// Ack the territory note only; the held tag must be untouched.
	runOK(t, tcCmdAck, territoryID)
	if out := runOK(t, tcCmdStatus); strings.Contains(out, "territory-tag") {
		t.Errorf("acked note must stop being live: %q", out)
	}
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if out := runOK(t, tcCmdStatus); !strings.Contains(out, heldID) {
		t.Errorf("acking a territory tag must not touch the held tag: %q", out)
	}
}

// TestE2E_TerritoryTagLifecycle: pin on unheld ground → the next agent arriving
// there is told → ack → the ground goes quiet → doctor is clean.
func TestE2E_TerritoryTagLifecycle(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	mkTerritory(t, repo)

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	id := tagID(t, runOK(t, tcCmdTag, ttDirPrefix, ttNoteText))

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if out := runOK(t, tcCmdLock, ttInsidePath, "-t", tcIntentTest); !strings.Contains(out, id) {
		t.Fatalf("arriving at the ground must deliver the note: %q", out)
	}
	runOK(t, tcCmdAck, id)
	if out := runOK(t, tcCmdRefresh, ttInsidePath); strings.Contains(out, "territory-tag") {
		t.Errorf("an acked note must stop surfacing: %q", out)
	}

	var doctorOut bytes.Buffer
	Run([]string{tcCmdDoctor}, &doctorOut, io.Discard)
	if strings.Contains(doctorOut.String(), "expired_territory_tag") {
		t.Errorf("a note that was read is not a finding: %q", doctorOut.String())
	}
}
