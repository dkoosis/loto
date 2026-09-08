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

// decideHeldRows is decideHeld's verdict half, for the cases below that have
// nothing unresolvable and no unlockable target. The ℹ-note half has its own
// tests (TestCheckHeld_StagedSymlink*, TestDecideHeld_Unlockable*).
func decideHeldRows(entries []stagedPath, locks []domain.LockRecord, claims []domain.ClaimRecord, myUUID string, ec domain.EvalContext) []heldRow {
	rows, _ := decideHeld(entries, nil, locks, claims, myUUID, ec)
	return rows
}

func TestDecideHeld_MySharedLockIsNotHeld(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Mode: domain.ModeShared, ExpiresAt: now.Add(time.Hour)},
	}
	rows := decideHeldRows([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
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
	rows := decideHeldRows([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].State != heldStateUnlocked {
		t.Fatalf("a beacon must not satisfy the gate: %+v", rows)
	}
}

func TestDecideHeld_MyExclusiveLockIsHeld(t *testing.T) {
	now := time.Now()
	locks := []domain.LockRecord{
		{Target: domain.Target{Canonical: tcTargetA}, OwnerUUID: gateMyUUID, Mode: domain.ModeExclusive, ExpiresAt: now.Add(time.Hour)},
	}
	if rows := decideHeldRows([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now)); len(rows) != 0 {
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
	rows := decideHeldRows([]stagedPath{{Path: target}}, nil, claims, gateMyUUID, ec)
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
	rows := decideHeldRows([]stagedPath{{Path: tcTargetA}}, locks, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].State != heldStateUnlocked {
		t.Fatalf("an expired peer lock is not a peer's: %+v", rows)
	}
}

func TestDecideHeld_DuplicatePathsCollapse(t *testing.T) {
	now := time.Now()
	entries := []stagedPath{{Path: tcTargetA}, {Path: tcTargetA}}
	if rows := decideHeldRows(entries, nil, nil, gateMyUUID, gateEC(now)); len(rows) != 1 {
		t.Fatalf("want one row for a repeated path, got %+v", rows)
	}
}

// ── loto-pgio: one odd staged name never switches the gate off ────────────
//
// The bug these pin was a full bypass, not a cosmetic one. resolveStagedPaths
// collected any path Canonicalize refused into `invalid` and the whole batch
// returned exit 2 — and the pre-commit leg proceeds on every exit but 1. One
// staged file with a quote in its name therefore disabled the ownership check
// for every OTHER path in that commit.

// AC 1: a staged name carrying a shell metacharacter is CHECKED, not refused.
// git printed it, no shell was involved, so the unexpanded-token rule that
// refuses it is answering a question nobody asked.
func TestCheckHeld_QuotedStagedNameIsCheckedAndNeverSuppressesTheOthers(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	const quoted = `say "hi".go`
	writeT(t, repo, quoted, "q")
	writeT(t, repo, tcTargetB, "b")
	gitT(t, repo, "--literal-pathspecs", "add", quoted, tcTargetB)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=2 unlocked=2 peer=0 staged=2\n" +
		"✗ path=b.go state=unlocked\n" +
		"✗ path=" + quoted + " state=unlocked\n" +
		tcHeldFence + "\n" +
		"loto lock 'b.go' 'say \"hi\".go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// AC 1, the other half: a staged name that STILL will not canonicalize —
// containment and glob syntax survive the git carve-out — is reported with its
// own reason and its own remedy, and the other staged paths are judged anyway.
func TestCheckHeld_UnresolvableStagedNameDoesNotSuppressTheOthers(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	const globbed = "a[1].go"
	writeT(t, repo, globbed, "g")
	writeT(t, repo, tcTargetB, "b")
	gitT(t, repo, "--literal-pathspecs", "add", globbed, tcTargetB)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=2 unlocked=1 peer=0 staged=2 unresolvable=1\n" +
		"✗ path=a[1].go state=unresolvable reason=glob-not-supported\n" +
		"✗ path=b.go state=unlocked\n" +
		tcHeldFence + "\n" +
		"loto lock 'b.go' -t \"<bead>: intent\"  # take what you are about to commit\n" +
		"git restore --staged 'a[1].go'  # loto cannot read these paths; leave them out of your commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// AC 2: a control character in the only staged name. The gate COMPLETES and
// reports on it — it used to exit 2 and take the whole commit out of scope.
// The row is Go-quoted so it cannot split the one-row-per-line surface, and
// no `loto lock` line is printed for it: there is no portable shell spelling
// of that token, and a remedy printed wrong is the defect this bead removes.
func TestCheckHeld_ControlCharacterNameIsReportedNotRefusedAsABatch(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	const tabbed = "a\tb.go"
	writeT(t, repo, tabbed, "t")
	gitT(t, repo, "--literal-pathspecs", "add", tabbed)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=1 peer=0 staged=1\n" +
		"✗ path=\"a\\tb.go\" state=unlocked\n" +
		"ℹ fix-omitted count=1 reason=control-character-in-name\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// A newline is the control character that would actually break a reader: it
// splits one row into two, and a parser would read the tail as a second path.
func TestCheckHeld_NewlineInAStagedNameDoesNotSplitTheRow(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, "new\nline.go", "n")
	gitT(t, repo, "--literal-pathspecs", "add", "new\nline.go")

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("want exit 1, got %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=1 peer=0 staged=1\n" +
		"✗ path=\"new\\nline.go\" state=unlocked\n" +
		"ℹ fix-omitted count=1 reason=control-character-in-name\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// ── loto-pgio: a target no session could ever hold ────────────────────────

// AC 4: `loto lock` refuses a symlink (reason=symlink), so demanding a lock on
// a staged symlink printed a remedy that cannot succeed and, in block mode,
// would refuse the commit forever. The gate exempts it and SAYS so — an ℹ row
// naming the path it is not protecting.
func TestCheckHeld_StagedSymlinkIsExemptAndSaysSo(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.Symlink(tcTargetA, filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "link.go")

	got, code := blockingRun(t)
	if code != 0 {
		t.Fatalf("a staged symlink alone must not refuse the commit; got exit %d: %q", code, got)
	}
	want := "✓ held count=1\n" +
		"ℹ path=link.go state=unlockable reason=symlink gate=not-protected\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
	if strings.Contains(got, tcCmdLock+" ") {
		t.Errorf("a remedy that cannot succeed must not be printed: %q", got)
	}
}

// AC 5: the same for a submodule pointer bump. The staged entry is a gitlink
// and the worktree path is the submodule's directory, which `loto lock` refuses
// with reason=not-regular-file — the exact token the ℹ row carries, because the
// gate asks lock's own validator rather than keeping a second list.
func TestCheckHeld_StagedSubmodulePointerIsExemptAndSaysSo(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.MkdirAll(filepath.Join(repo, "vendored"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A gitlink without a real submodule checkout: the index entry is what the
	// gate reads, and the directory on disk is what it stats.
	gitT(t, repo, "update-index", "--add", "--cacheinfo", "160000,4b825dc642cb6eb9a060e54bf8d69288fbee4904,vendored")

	got, code := blockingRun(t)
	if code != 0 {
		t.Fatalf("a staged submodule pointer alone must not refuse the commit; got exit %d: %q", code, got)
	}
	want := "✓ held count=1\n" +
		"ℹ path=vendored state=unlockable reason=not-regular-file gate=not-protected\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// The exemption is scoped to the UNLOCKED verdict. A peer's lock on a path
// that is a symlink today — they took it when it was a regular file — is still
// real and still named, because `git restore --staged` is a remedy that works.
func TestCheckHeld_PeerLockOnAnUnlockableTargetIsStillNamed(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	if code := Run([]string{tcCmdLock, tcTargetA, "-t", tcIntentTest}, &bytes.Buffer{}, &bytes.Buffer{}); code != 0 {
		t.Fatalf("alice lock %s failed", tcTargetA)
	}
	// a.go becomes a symlink under alice's live lock.
	if err := os.Remove(filepath.Join(repo, tcTargetA)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere.go", filepath.Join(repo, tcTargetA)); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	gitT(t, repo, "add", tcTargetA)

	got, code := blockingRun(t)
	if code != 1 {
		t.Fatalf("a peer's lock is still a refusal; got exit %d: %q", code, got)
	}
	want := "✗ unheld count=1 unlocked=0 peer=1 staged=1\n" +
		"✗ path=a.go state=peer-lock blocker=" + alice.UUID + " intent=\"test\" expires_at=<T>\n" +
		tcHeldFence + "\n" +
		"git restore --staged 'a.go'  # a peer holds these; leave them out of your commit\n" +
		"```\n"
	if got != want {
		t.Errorf("output\n got: %q\nwant: %q", got, want)
	}
}

// An exempt path is not a firing: the counter measures what the gate WOULD
// refuse, and it would never refuse this one.
func TestCheckHeld_ExemptPathDoesNotFireTheCounter(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.Symlink(tcTargetA, filepath.Join(repo, "link.go")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "link.go")

	before := countFiringEvents(t)
	if _, code := heldRun(t); code != 0 {
		t.Fatalf("want exit 0, got %d", code)
	}
	if after := countFiringEvents(t); after != before {
		t.Errorf("an exempt path is not a firing: got %d, want %d", after, before)
	}
}

// decideHeld's half of the exemption, without the filesystem.
func TestDecideHeld_UnlockableIsANoteNotAVerdict(t *testing.T) {
	now := time.Now()
	entries := []stagedPath{{Path: "link.go", Unlockable: "symlink"}, {Path: tcTargetB}}
	rows, notes := decideHeld(entries, nil, nil, nil, gateMyUUID, gateEC(now))
	if len(rows) != 1 || rows[0].Path != tcTargetB {
		t.Fatalf("only the lockable path is a verdict row: %+v", rows)
	}
	if len(notes) != 1 || notes[0].State != heldStateUnlockable || notes[0].Reason != "symlink" {
		t.Fatalf("the unlockable path is an ℹ note: %+v", notes)
	}
}

// An unresolvable path is a verdict row, not a note: the gate could not
// complete a check it was asked for, and unstaging the path is a real remedy.
func TestDecideHeld_UnresolvableIsAVerdictRow(t *testing.T) {
	now := time.Now()
	rows, notes := decideHeld(nil, []checkInvalid{{Path: "odd", Reason: "glob-not-supported"}}, nil, nil, gateMyUUID, gateEC(now))
	if len(notes) != 0 {
		t.Fatalf("want no notes: %+v", notes)
	}
	if len(rows) != 1 || rows[0].State != heldStateUnresolvable || rows[0].Reason != "glob-not-supported" {
		t.Fatalf("want one unresolvable row: %+v", rows)
	}
}

// ── loto-l9ve: --held honors --cwd-unknown ────────────────────────────────

// The flag exists for callers whose working directory is not knowable (an MCP
// shell). --held dropped it, so a relative token silently resolved against
// loto's own cwd and could report a same-named file in another directory as
// held. It is refused now, with the same reason and exit the ordinary check
// route gives.
func TestCheckHeld_CwdUnknownRefusesRelativePath(t *testing.T) {
	withTempProject(t)
	pinAgent(t)
	var heldOut, heldErr bytes.Buffer
	heldCode := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagCwdUnknown, "some/relative/path.go"}, &heldOut, &heldErr)

	var plainOut, plainErr bytes.Buffer
	plainCode := Run([]string{tcCmdCheck, tcFlagCwdUnknown, "some/relative/path.go"}, &plainOut, &plainErr)

	if heldCode != plainCode {
		t.Errorf("exit: --held gave %d, the ordinary route gives %d", heldCode, plainCode)
	}
	if heldOut.String() != plainOut.String() {
		t.Errorf("refusal differs from the ordinary route\n held: %q\nplain: %q", heldOut.String(), plainOut.String())
	}
	if !strings.Contains(heldOut.String(), "reason=relative-path-caller-cwd-unknown") {
		t.Errorf("want the caller-cwd-unknown reason: %q", heldOut.String())
	}
}

// --staged paths come from git run with cmd.Dir=repoTop, so they are
// repo-root-relative by construction: refusing them would make
// `--cwd-unknown --staged` reject every nonempty commit.
func TestCheckHeld_CwdUnknownWithStagedStillWorks(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	writeT(t, repo, tcTargetB, "b")
	gitT(t, repo, "add", tcTargetB)

	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagCwdUnknown, tcFlagStaged}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want the advisory exit 0, got %d: %q", code, out.String())
	}
	if !strings.HasPrefix(out.String(), "⚠ unheld") {
		t.Errorf("want the staged path judged: %q", out.String())
	}
}

// An absolute path carries its own base, so --cwd-unknown has nothing to
// refuse and the check runs.
func TestCheckHeld_CwdUnknownAbsolutePathStillWorks(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	var out, errBuf bytes.Buffer
	code := Run([]string{tcCmdCheck, tcFlagHeld, tcFlagCwdUnknown, filepath.Join(repo, tcTargetA)}, &out, &errBuf)
	if code != 0 {
		t.Fatalf("want the advisory exit 0, got %d: %q out=%q err=%q", code, out.String(), out.String(), errBuf.String())
	}
	if !strings.HasPrefix(out.String(), "⚠ unheld") {
		t.Errorf("want the absolute path judged: %q", out.String())
	}
}
