package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// Golden tests for `loto check --held` (loto-7oik). Every case the bead's
// acceptance criteria name is here, each asserting the WHOLE stdout block
// byte for byte rather than a substring: the output is a hook's refusal
// message and a fix block a committer copy-pastes, so a silently reordered
// row or a dropped fix line is a real regression.
//
// expires_at is the one field a golden cannot pin — it is now+TTL. scrubTime
// replaces it with a fixed token, and TestCheckHeld_OutputIsByteIdentical
// covers the determinism the field would otherwise have tested.

const (
	tcFlagHeld  = "--held"
	tcHeldFence = "```bash"
)

// scrubTime rewrites every `expires_at=<RFC3339>` to `expires_at=<T>` so a
// golden can assert the rest of the line exactly.
func scrubTime(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "expires_at=")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		b.WriteString("expires_at=<T>")
		rest := s[i+len("expires_at="):]
		// The timestamp runs to the next space or newline.
		j := strings.IndexAny(rest, " \n")
		if j < 0 {
			return b.String()
		}
		s = rest[j:]
	}
}

// gitT runs git in dir and fails the test on error.
func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// writeT creates dir/name with content.
func writeT(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// heldRun executes `loto check --held --staged` in whatever mode the
// environment currently says, and returns scrubbed stdout plus the exit code.
func heldRun(t *testing.T) (string, int) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagStaged}, &out, &errBuf)
	t.Logf("stderr: %q", errBuf.String())
	return scrubTime(out.String()), code
}

// blockingRun is heldRun with the gate asked to refuse. The AC cases below
// describe what the gate REFUSES and what it prints; the mode it ships in is
// a separate question, pinned by TestCheckHeld_DefaultModeIsAdvisory. Asking
// for blocking here keeps the two independent, so flipping the shipped
// default can never quietly gut the refusal tests.
func blockingRun(t *testing.T) (string, int) {
	t.Helper()
	t.Setenv(gateModeEnv, gateModeBlock)
	return heldRun(t)
}

// countFiringEvents reads the advisory-first firing counter straight out of
// loto's events log.
func countFiringEvents(t *testing.T) int {
	t.Helper()
	rt, err := openRuntime(context.Background())
	if err != nil {
		t.Fatalf("openRuntime: %v", err)
	}
	defer rt.Close()
	evs, err := rt.Store.ListEvents(rt.Ctx)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	n := 0
	for i := range evs {
		if evs[i].Kind == store.EventStagedGateFired {
			n++
		}
	}
	return n
}

// AC 1: session holds a lock on A only, stages A and B → refused, B named
// unlocked, with a `loto lock` fix block naming B.
func TestCheckHeld_UnlockedStagedPathRefuses(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	gitT(t, repo, "add", tcTargetA, tcTargetB)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=1 peer=0 staged=2\n" +
		"✗ path=b.go state=unlocked\n" +
		tcHeldFence + "\n" +
		"loto lock 'b.go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// AC 2: same shape, but B is held by a live peer → the peer is named and the
// fix is `git restore --staged`, not `loto lock`.
func TestCheckHeld_PeerLockedStagedPathNamesTheHolder(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	writeT(t, repo, tcTargetB, "b")

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid())) // durable + alive
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock %s failed", tcTargetB)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("bob lock %s failed", tcTargetA)
	}
	gitT(t, repo, "add", tcTargetA, tcTargetB)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=0 peer=1 staged=2\n" +
		"✗ path=b.go state=peer-lock blocker=" + alice.UUID + " intent=\"test\" expires_at=<T>\n" +
		tcHeldFence + "\n" +
		"git restore --staged 'b.go'  # a peer holds these; leave them out of your commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// AC 3: a staged rename A→C with only A locked is refused, and BOTH paths
// appear — the destination row carries renamed_from so the source is named
// even though it is held.
func TestCheckHeld_StagedRenameNeedsBothSidesLocked(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	// A rename is only detectable against a commit, so A has to be in HEAD.
	gitT(t, repo, "add", tcTargetA)
	gitT(t, repo, "commit", "-q", "-m", "seed")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	if err := os.Rename(filepath.Join(repo, tcTargetA), filepath.Join(repo, tcTargetC)); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A", tcTargetA, tcTargetC)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=1 peer=0 staged=2\n" +
		"✗ path=c.go state=unlocked renamed_from=a.go\n" +
		tcHeldFence + "\n" +
		"loto lock 'c.go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}

	// ...and locking the destination too clears it.
	if code := Run([]string{tcCmdLock, tcTargetC, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetC)
	}
	got, code = heldRun(t)
	if code != 0 || got != "✓ held count=2\n" {
		t.Errorf("both sides locked must pass: code=%d out=%q", code, got)
	}
}

// AC 4: LOTO_GATE_MODE=warn renders the same rows as ⚠, exits 0, and still
// increments the firing counter in the events log.
func TestCheckHeld_WarnModeAdvisesAndCountsTheFiring(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	gitT(t, repo, "add", tcTargetA, tcTargetB)

	before := countFiringEvents(t)
	t.Setenv(gateModeEnv, gateModeWarn)
	got, code := heldRun(t)
	if code != 0 {
		t.Fatalf("warn mode must exit 0, got %d: %q", code, got)
	}
	want := "⚠ unheld count=1 unlocked=1 peer=0 staged=2\n" +
		"⚠ path=b.go state=unlocked\n" +
		tcHeldFence + "\n" +
		"loto lock 'b.go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
	if after := countFiringEvents(t); after != before+1 {
		t.Errorf("firing counter: got %d, want %d", after, before+1)
	}
}

// The warn rows and the blocking rows differ in the glyph and nothing else —
// the property that makes "same output as ⚠ rows" structural rather than a
// second formatter that can drift.
func TestCheckHeld_WarnDiffersFromBlockOnlyByTheGlyph(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	gitT(t, repo, "add", tcTargetA, tcTargetB)

	blocked, blockCode := blockingRun(t)
	t.Setenv(gateModeEnv, gateModeWarn)
	warned, warnCode := heldRun(t)
	if blockCode != 1 || warnCode != 0 {
		t.Fatalf("codes: block=%d warn=%d", blockCode, warnCode)
	}
	if strings.ReplaceAll(warned, "⚠", "✗") != blocked {
		t.Errorf("warn output is not the blocking output with a different glyph\n warn: %q\nblock: %q", warned, blocked)
	}
}

// Determinism (.claude/rules/design.md): the same store state renders
// byte-identically across reads.
func TestCheckHeld_OutputIsByteIdentical(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	writeT(t, repo, tcTargetB, "b")
	writeT(t, repo, tcTargetC, "c")
	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetB, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock failed")
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	gitT(t, repo, "add", tcTargetA, tcTargetB, tcTargetC)

	first, code1 := heldRun(t)
	second, code2 := heldRun(t)
	if code1 != code2 || first != second {
		t.Errorf("not byte-identical across reads:\n1: %q (%d)\n2: %q (%d)", first, code1, second, code2)
	}
	if !strings.Contains(first, "unheld count=3") {
		t.Errorf("want all three staged paths reported unheld: %q", first)
	}
}

// Nothing staged is not a refusal.
func TestCheckHeld_NothingStagedPasses(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	got, code := heldRun(t)
	if code != 0 || got != "✓ no paths\n" {
		t.Errorf("empty index: code=%d out=%q", code, got)
	}
}

// THE SHIPPED DEFAULT. With LOTO_GATE_MODE unset an unheld staged path is an
// advisory: ⚠ rows, exit 0, and the firing counter still moves. The epic's
// Rules ("New gates ship in warn mode and promote to blocking on evidence")
// and its own acceptance criteria ("session staging an unlocked file gets a
// ⚠ warn row and the commit proceeds") both say so, and this is the test
// that fails if a later change flips the default without deciding to.
func TestCheckHeld_DefaultModeIsAdvisory(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("lock %s failed", tcTargetA)
	}
	gitT(t, repo, "add", tcTargetA, tcTargetB)

	// Explicitly UNSET, not merely absent: another test in this package may
	// have exported it, and the default is the whole subject here.
	t.Setenv(gateModeEnv, "")
	before := countFiringEvents(t)
	got, code := heldRun(t)
	if code != 0 {
		t.Fatalf("the shipped default must not refuse a commit; got exit %d: %q", code, got)
	}
	want := "⚠ unheld count=1 unlocked=1 peer=0 staged=2\n" +
		"⚠ path=b.go state=unlocked\n" +
		tcHeldFence + "\n" +
		"loto lock 'b.go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
	if after := countFiringEvents(t); after != before+1 {
		t.Errorf("the advisory period is worthless without the counter: got %d, want %d", after, before+1)
	}
}

// An unrecognized LOTO_GATE_MODE falls back to the shipped default and names
// the value. Blocking is opted into by name; a string nobody meant must not
// be able to start refusing commits.
func TestCheckHeld_UnknownGateModeFallsBackToWarn(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	gitT(t, repo, "add", tcTargetB)
	t.Setenv(gateModeEnv, "advisory")
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagStaged}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want exit 0 on an unrecognized mode, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "⚠ LOTO_GATE_MODE=\"advisory\" unrecognized gate=warn") {
		t.Errorf("want an unrecognized-mode notice on stderr: %q", errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "⚠ unheld") {
		t.Errorf("want advisory rows: %q", out.String())
	}
}

// LOTO_GATE_MODE=block is what turns the advisory into a refusal, and it must
// keep working from any starting state.
func TestCheckHeld_BlockModeRefuses(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	gitT(t, repo, "add", tcTargetB)
	t.Setenv(gateModeEnv, gateModeBlock)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagStaged}, &out, &errBuf); code != 1 {
		t.Fatalf("LOTO_GATE_MODE=%s must refuse; got exit %d out=%q", gateModeBlock, code, out.String())
	}
	if !strings.HasPrefix(out.String(), "✗ unheld") {
		t.Errorf("want blocking rows: %q", out.String())
	}
}

// --held and --gate ask opposite questions; combining them is refused rather
// than answered about one of the two.
func TestCheckHeld_RefusesGateCombination(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagGate, tcFlagStaged}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
	if !strings.Contains(errBuf.String(), "opposite questions") {
		t.Errorf("want a refusal naming the conflict: %q", errBuf.String())
	}
}

func TestCheckHeld_RefusesWithNoPaths(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	if code := Run([]string{tcCmdCheck, tcFlagHeld}, &out, &errBuf); code != 2 {
		t.Fatalf("want exit 2, got %d", code)
	}
}

// ── parseStagedNameStatus: the NUL framing ────────────────────────────────

func TestParseStagedNameStatus_RenameYieldsBothSides(t *testing.T) {
	got := parseStagedNameStatus("R100\x00a.go\x00c.go\x00M\x00d.go\x00")
	want := []stagedPath{{Path: tcTargetA}, {Path: tcTargetC, RenamedFrom: tcTargetA}, {Path: "d.go"}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %+v, want %+v", i, got[i], want[i])
		}
	}
}

// A path containing a space, a quote, or a newline must survive intact. Any
// of the three would be mangled by the non-NUL form of this command, and a
// newline would split one path into two bogus entries.
func TestParseStagedNameStatus_HostilePathsSurvive(t *testing.T) {
	got := parseStagedNameStatus("A\x00a b.go\x00A\x00q\"uote.go\x00A\x00new\nline.go\x00")
	want := []string{"a b.go", "q\"uote.go", "new\nline.go"}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %v", got, want)
	}
	for i := range want {
		if got[i].Path != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i].Path, want[i])
		}
	}
}

func TestParseStagedNameStatus_CopyIsRenameShaped(t *testing.T) {
	got := parseStagedNameStatus("C75\x00a.go\x00b.go\x00")
	if len(got) != 2 || got[1].RenamedFrom != tcTargetA {
		t.Fatalf("copy must yield both sides: %+v", got)
	}
}

func TestParseStagedNameStatus_TruncatedTailIsDropped(t *testing.T) {
	if got := parseStagedNameStatus("M\x00a.go\x00R100\x00b.go\x00"); len(got) != 1 || got[0].Path != tcTargetA {
		t.Fatalf("truncated rename must be dropped, not guessed: %+v", got)
	}
}

func TestParseStagedNameStatus_EmptyIsEmpty(t *testing.T) {
	if got := parseStagedNameStatus(""); len(got) != 0 {
		t.Fatalf("want no entries, got %+v", got)
	}
}

// ── decideHeld: the pure verdict ──────────────────────────────────────────

func TestDecideHeld_MySharedLockIsNotHeld(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Mode: domain.ModeShared, ExpiresAt: now.Add(time.Hour)},
	}
	rows := decideHeld([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].State != heldStateUnlocked {
		t.Fatalf("a shared self-lock is a read declaration, not a write claim: %+v", rows)
	}
}

func TestDecideHeld_MyBeaconIsNotHeld(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Mode: domain.ModeShared,
			Intent: gateIntentBeacon, ExpiresAt: now.Add(time.Hour), Beacon: true},
	}
	rows := decideHeld([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].State != heldStateUnlocked {
		t.Fatalf("a beacon must not satisfy the gate: %+v", rows)
	}
}

func TestDecideHeld_MyExclusiveLockIsHeld(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Mode: domain.ModeExclusive, ExpiresAt: now.Add(time.Hour)},
	}
	if rows := decideHeld([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now)); len(rows) != 0 {
		t.Fatalf("my own exclusive lock must satisfy the gate: %+v", rows)
	}
}

func TestDecideHeld_PeerClaimIsNamedNotReportedUnlocked(t *testing.T) {
	now := time.Now()
	target := tcPrefixStore + "/new.go"
	claims := []domain.ClaimRecord{
		{PathPrefix: tcPrefixStore, OwnerUUID: gateFoeUUID, Intent: gateIntentFoe, ExpiresAt: now.Add(time.Hour)},
	}
	ec := gateEC(now)
	ec.Live = aliveProbe
	rows := decideHeld([]stagedPath{{Path: target}}, nil, claims, gateMyUUID, ec)
	if len(rows) != 1 || rows[0].State != heldStatePeerClaim || rows[0].HolderUUID != gateFoeUUID {
		t.Fatalf("a peer's covering claim must be named: %+v", rows)
	}
	if rows[0].BlockerPath != tcPrefixStore {
		t.Errorf("claim row must carry the prefix: %+v", rows[0])
	}
}

// A stale peer lock — one `loto lock` would silently reclaim — must not be
// reported as a peer's; the path is simply unlocked.
func TestDecideHeld_StalePeerLockReadsUnlocked(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateFoeUUID, Mode: domain.ModeExclusive, ExpiresAt: now.Add(-time.Minute)},
	}
	rows := decideHeld([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].State != heldStateUnlocked {
		t.Fatalf("an expired peer lock is not a peer's: %+v", rows)
	}
}

func TestDecideHeld_DuplicatePathsCollapse(t *testing.T) {
	now := time.Now()
	entries := []stagedPath{{Path: tcTargetA}, {Path: tcTargetA}}
	if rows := decideHeld(entries, nil, nil, gateMyUUID, gateEC(now)); len(rows) != 1 {
		t.Fatalf("want one row for a repeated path, got %+v", rows)
	}
}
