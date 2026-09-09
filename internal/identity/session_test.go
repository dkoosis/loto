package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestRecordSessionWritesWitnesses: inside a session, whoami's record carries
// the session id, the owner, and the pid + start-time of the long-lived
// session process (LOTO_PID), published under ~/.loto/session/<sid>.json.
func TestRecordSessionWritesWitnesses(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	t.Setenv("LOTO_PID", strconv.Itoa(os.Getpid()))
	socket := existingSocket(t)
	t.Setenv("CLAUDE_CODE_MESSAGING_SOCKET", socket)

	rec, err := RecordSession(&Agent{UUID: tcSessionA, Host: "h"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SessionID != tcSessionA || rec.UUID != tcSessionA || rec.PID != os.Getpid() || rec.Socket != socket {
		t.Errorf("record = %+v", rec)
	}
	if want, ok := ProcStart(os.Getpid()); ok && rec.ProcStart != want {
		t.Errorf("proc_start = %d, want %d", rec.ProcStart, want)
	}

	body, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".loto", "session", tcSessionA+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var back SessionRecord
	if err := json.Unmarshal(body, &back); err != nil {
		t.Fatal(err)
	}
	if back.SessionID != tcSessionA || back.PID != rec.PID {
		t.Errorf("round-trip = %+v", back)
	}

	// A second call from the same session replaces the record, never a
	// second file.
	if _, err := RecordSession(&Agent{UUID: tcSessionA}); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(sessionDir()); len(entries) != 1 {
		t.Errorf("want 1 record, got %d", len(entries))
	}
}

// TestRecordSessionHonorsLOTOBase: LOTO_BASE redirects the session dir with no
// ".loto" segment (sd-kx5), so a test or isolated run never writes into the
// real home.
func TestRecordSessionHonorsLOTOBase(t *testing.T) {
	clearIdentityEnv(t)
	base := t.TempDir()
	t.Setenv("LOTO_BASE", base)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)

	if _, err := RecordSession(&Agent{UUID: tcSessionA}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "session", tcSessionA+".json")); err != nil {
		t.Errorf("record not under LOTO_BASE/session: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(os.Getenv("HOME"), ".loto")); len(entries) != 0 {
		t.Errorf("wrote into HOME despite LOTO_BASE: %d entries", len(entries))
	}
}

func TestRecordSessionOutsideASessionIsNoop(t *testing.T) {
	clearIdentityEnv(t)
	rec, err := RecordSession(Ephemeral())
	if rec != nil || err != nil {
		t.Errorf("no session id: got (%+v, %v), want (nil, nil)", rec, err)
	}
	if _, err := os.Stat(sessionDir()); !os.IsNotExist(err) {
		t.Errorf("session dir must not be created on a bare shell: %v", err)
	}
}

// TestRecordSessionKeyedBySessionIDFromEnv: the record file is keyed by the
// same precedence lock rows stamp their session id with (LOTO_SESSION_ID
// over CLAUDE_CODE_SESSION_ID), so ProbeSession(row.SessionUUID) finds it by
// construction (loto-37xm).
func TestRecordSessionKeyedBySessionIDFromEnv(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("CLAUDE_CODE_SESSION_ID", tcSessionA)
	t.Setenv("LOTO_SESSION_ID", "override-1")

	rec, err := RecordSession(&Agent{UUID: tcSessionA})
	if err != nil {
		t.Fatal(err)
	}
	if rec.SessionID != "override-1" {
		t.Errorf("session id = %q, want the LOTO_SESSION_ID override", rec.SessionID)
	}
	if _, ok := readSession("override-1"); !ok {
		t.Error("record not readable under the override key")
	}
}

func TestSessionPID(t *testing.T) {
	clearIdentityEnv(t)
	unsetEnv(t, "LOTO_PID")
	unsetEnv(t, "CLAUDE_PID")
	if got := sessionPID(""); got != 0 {
		t.Errorf("nothing set: %d, want 0", got)
	}
	if got := sessionPID("/tmp/cc-socks/63879.sock"); got != 63879 {
		t.Errorf("socket basename: %d, want 63879", got)
	}
	t.Setenv("CLAUDE_PID", "42")
	if got := sessionPID("/tmp/cc-socks/63879.sock"); got != 42 {
		t.Errorf("CLAUDE_PID must beat the socket: %d", got)
	}
	t.Setenv("LOTO_PID", "7")
	if got := sessionPID(""); got != 7 {
		t.Errorf("LOTO_PID must win: %d", got)
	}
	t.Setenv("LOTO_PID", "garbage")
	if got := sessionPID(""); got != 42 {
		t.Errorf("invalid LOTO_PID must fall through: %d", got)
	}
}

// --- LiveOwnerUUIDs -------------------------------------------------------------

// plantWitnessedSession writes a record whose liveness is fully controlled by
// pid, bypassing RecordSession: pid=os.Getpid() verdicts live (this test
// process is always alive), any other positive pid verdicts dead once the OS
// confirms it isn't running. No socket, so socket-missing never fires and the
// pid witness is the only thing LiveOwnerUUIDs' Verdict() call can read.
func plantWitnessedSession(t *testing.T, sid, owner string, pid int) {
	t.Helper()
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(SessionRecord{SessionID: sid, UUID: owner, PID: pid, RecordedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir(), sid+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestLiveOwnerUUIDs is loto-9spo's evidence that a session record answers
// "is this owner alive" with no lock row involved: scanDanglingStashes has
// only an owner uuid from a stash's LOTO_AGENT_ID stamp, never a lock to key
// ProbeSession(sid) from.
func TestLiveOwnerUUIDs(t *testing.T) {
	clearIdentityEnv(t)
	plantWitnessedSession(t, "live-sess", "owner-live", os.Getpid())
	plantWitnessedSession(t, "dead-sess", "owner-dead", 999999999) // implausible pid, reads not-running
	plantSession(t, "unknown-sess", "owner-unknown", time.Hour)    // no pid, no socket → SessionUnknown

	got := LiveOwnerUUIDs()
	if _, ok := got["owner-live"]; !ok {
		t.Errorf("owner-live missing from %v, want present (its session verdicts live)", got)
	}
	if _, ok := got["owner-dead"]; ok {
		t.Errorf("owner-dead present in %v, want absent (its session verdicts dead)", got)
	}
	if _, ok := got["owner-unknown"]; ok {
		t.Errorf("owner-unknown present in %v, want absent — unknown is not evidence of life", got)
	}
}

// TestLiveOwnerUUIDsAnyLiveSiblingCounts: one owner uuid can carry several
// session records (sibling sessions, loto-81n). LiveOwnerUUIDs answers "is
// this owner doing anything anywhere", so one live sibling is enough even
// while another of the same owner's sessions has died.
func TestLiveOwnerUUIDsAnyLiveSiblingCounts(t *testing.T) {
	clearIdentityEnv(t)
	plantWitnessedSession(t, "sib-dead", "shared-owner", 999999999)
	plantWitnessedSession(t, "sib-live", "shared-owner", os.Getpid())

	got := LiveOwnerUUIDs()
	if _, ok := got["shared-owner"]; !ok {
		t.Errorf("shared-owner missing from %v, want present — one live sibling is enough", got)
	}
}

func TestLiveOwnerUUIDsMissingDirIsEmptyNotError(t *testing.T) {
	clearIdentityEnv(t)
	t.Setenv("LOTO_BASE", filepath.Join(t.TempDir(), "does-not-exist"))
	if got := LiveOwnerUUIDs(); len(got) != 0 {
		t.Errorf("missing session dir: got %v, want empty", got)
	}
}

// --- LiveOwnerUUIDs as S (loto-ea8y.3, enforcement-design.md §3) ----------
//
// S ("live sessions in one checkout") is the set I1 (§5) refuses a protected
// ref transition under whenever |S| >= 2. LiveOwnerUUIDs is that query:
// membership comes from identity.SessionRecord + the PID/ProcStart probe
// alone, never from a call record, so an idle session between calls stays a
// member. No store table backs it (§3 Givens: reuse SessionRecord, add
// nothing) — and internal/store cannot import internal/identity (arch-lint),
// so this query lives here, not in package store; the ref hook (loto-ea8y.4)
// and `loto doctor` (already wired, doctor_stash.go) both call it directly,
// the same way runtime.go's liveProbe already crosses the identity/cli
// boundary for lock liveness.

// plantWitnessedSessionProcStart is plantWitnessedSession plus an explicit
// ProcStart, for the mismatch case below (plantWitnessedSession always
// leaves it at the zero value).
func plantWitnessedSessionProcStart(t *testing.T, sid, owner string, pid int, procStart int64) {
	t.Helper()
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(SessionRecord{SessionID: sid, UUID: owner, PID: pid, ProcStart: procStart, RecordedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir(), sid+".json"), body, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestS_TwoLiveSessionsNoCallRecords pins spec test 13's query half and this
// bead's first AC: two session records with live pids, nothing about a call
// record anywhere (LiveOwnerUUIDs never reads one), yield |S| = 2.
func TestS_TwoLiveSessionsNoCallRecords(t *testing.T) {
	clearIdentityEnv(t)
	plantWitnessedSession(t, "s1", "owner-1", os.Getpid())
	plantWitnessedSession(t, "s2", "owner-2", os.Getpid())

	got := LiveOwnerUUIDs()
	if len(got) != 2 {
		t.Fatalf("|S| = %d (%v), want 2", len(got), got)
	}
	for _, owner := range []string{"owner-1", "owner-2"} {
		if _, ok := got[owner]; !ok {
			t.Errorf("%s missing from S = %v", owner, got)
		}
	}
}

// TestS_ExcludesDeadPidAndProcStartMismatch pins this bead's second AC: a
// record whose pid is dead, or whose ProcStart no longer matches the pid's
// current OS start-time, is excluded from S — while a live, matching record
// stays in.
func TestS_ExcludesDeadPidAndProcStartMismatch(t *testing.T) {
	clearIdentityEnv(t)
	stubProcStart(t, 500, true) // any pid probed in this test reads start-time 500

	plantWitnessedSessionProcStart(t, "s-live", "owner-live", os.Getpid(), 500)         // matches -> live
	plantWitnessedSession(t, "s-dead-pid", "owner-dead-pid", deadPID(t))                // pid gone -> dead
	plantWitnessedSessionProcStart(t, "s-mismatch", "owner-mismatch", os.Getpid(), 999) // 999 != 500 -> dead (recycled pid)

	got := LiveOwnerUUIDs()
	if _, ok := got["owner-live"]; !ok {
		t.Errorf("owner-live missing from S = %v, want present", got)
	}
	if _, ok := got["owner-dead-pid"]; ok {
		t.Errorf("owner-dead-pid present in S = %v, want excluded (pid dead)", got)
	}
	if _, ok := got["owner-mismatch"]; ok {
		t.Errorf("owner-mismatch present in S = %v, want excluded (ProcStart mismatch)", got)
	}
	if len(got) != 1 {
		t.Errorf("|S| = %d (%v), want 1", len(got), got)
	}
}

// TestS_ProcStartZeroWithLivePidIncluded pins this bead's third AC: 0 means
// unknown, never dead (§3 Givens) — a record with ProcStart 0 and a live pid
// is a member of S.
func TestS_ProcStartZeroWithLivePidIncluded(t *testing.T) {
	clearIdentityEnv(t)
	plantWitnessedSessionProcStart(t, "s-zero", "owner-zero-procstart", os.Getpid(), 0)

	got := LiveOwnerUUIDs()
	if _, ok := got["owner-zero-procstart"]; !ok {
		t.Errorf("owner-zero-procstart missing from S = %v, want present (ProcStart 0 is unknown, not dead)", got)
	}
}

// --- GCSessions ---------------------------------------------------------------

// plantSession writes a session record file for sid with the given owner and
// mtime, bypassing RecordSession so the test controls age.
func plantSession(t *testing.T, sid, owner string, age time.Duration) string {
	t.Helper()
	if err := os.MkdirAll(sessionDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir(), sid+".json")
	body, err := json.Marshal(SessionRecord{SessionID: sid, UUID: owner, RecordedAt: time.Now().Add(-age)})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-age)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestGCSessionsReapsStaleAndKeepsFresh(t *testing.T) {
	clearIdentityEnv(t)
	stale := plantSession(t, "stale", "o1", sessionGCMaxAge+time.Hour)
	fresh := plantSession(t, "fresh", "o2", time.Hour)

	reaped, residual, err := GCSessions(time.Now(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 || residual != 0 {
		t.Errorf("reaped=%d residual=%d, want 1/0", reaped, residual)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale record must be gone: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh record must survive: %v", err)
	}
}

func TestGCSessionsKeepsCallersOwnSessionAndLockPinned(t *testing.T) {
	clearIdentityEnv(t)
	mine := plantSession(t, "mine", "o-mine", sessionGCMaxAge+time.Hour)
	pinned := plantSession(t, "pinned", "o-pinned", sessionGCMaxAge+time.Hour)
	other := plantSession(t, "other", "o-other", sessionGCMaxAge+time.Hour)

	reaped, _, err := GCSessions(time.Now(), "mine", map[string]struct{}{"o-pinned": {}})
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 1 {
		t.Errorf("reaped=%d, want 1", reaped)
	}
	for _, keep := range []string{mine, pinned} {
		if _, err := os.Stat(keep); err != nil {
			t.Errorf("%s must survive: %v", filepath.Base(keep), err)
		}
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Errorf("unpinned stale record must be gone: %v", err)
	}
}

func TestGCSessionsBoundedByMaxUnlink(t *testing.T) {
	clearIdentityEnv(t)
	prev := sessionGCMaxUnlink
	sessionGCMaxUnlink = 2
	t.Cleanup(func() { sessionGCMaxUnlink = prev })
	for _, sid := range []string{"s1", "s2", "s3", "s4"} {
		plantSession(t, sid, "o", sessionGCMaxAge+time.Hour)
	}
	reaped, residual, err := GCSessions(time.Now(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if reaped != 2 || residual != 2 {
		t.Errorf("reaped=%d residual=%d, want 2/2", reaped, residual)
	}
}

func TestGCSessionsSurvivesMissingDir(t *testing.T) {
	clearIdentityEnv(t)
	if _, _, err := GCSessions(time.Now(), "", nil); err == nil {
		t.Error("missing dir must surface as an error for the caller to ignore, got nil")
	}
	// The marker-gated wrapper swallows nothing but must not panic either.
	if ran, _, _, _ := GCSessionsIfDue(time.Now(), "", nil); !ran {
		t.Error("first GCSessionsIfDue must run")
	}
}

func TestGCSessionsIfDueIsMarkerGated(t *testing.T) {
	clearIdentityEnv(t)
	plantSession(t, "stale", "o", sessionGCMaxAge+time.Hour)
	now := time.Now()
	if ran, reaped, _, _ := GCSessionsIfDue(now, "", nil); !ran || reaped != 1 {
		t.Fatalf("first pass ran=%v reaped=%d, want true/1", ran, reaped)
	}
	plantSession(t, "stale2", "o", sessionGCMaxAge+time.Hour)
	if ran, _, _, _ := GCSessionsIfDue(now.Add(time.Minute), "", nil); ran {
		t.Error("second pass inside the interval must be skipped")
	}
	if ran, reaped, _, _ := GCSessionsIfDue(now.Add(sessionGCMinInterval+time.Minute), "", nil); !ran || reaped != 1 {
		t.Errorf("pass after the interval ran=%v reaped=%d, want true/1", ran, reaped)
	}
}

// --- home.go ------------------------------------------------------------------

func TestSyncDir(t *testing.T) {
	if err := syncDir(t.TempDir()); err != nil {
		t.Fatalf("syncDir on real dir: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Fatal("syncDir on missing path: want error, got nil")
	}
}

// TestMkdirAllSync asserts the create-then-fsync-parent helper: it creates a
// missing directory, is a no-op on a pre-existing one, and surfaces the real
// error when the path exists as a non-directory (no error masking) — the
// MkdirAll-site half of loto-4n65. Power-loss durability is not observable
// from userspace (see TestSyncDir), so this covers only the control flow.
func TestMkdirAllSync(t *testing.T) {
	base := t.TempDir()

	fresh := filepath.Join(base, "fresh")
	if err := mkdirAllSync(fresh); err != nil {
		t.Fatalf("mkdirAllSync on missing dir: %v", err)
	}
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() {
		t.Fatalf("mkdirAllSync did not create dir: stat=%v err=%v", fi, err)
	}
	if err := mkdirAllSync(fresh); err != nil {
		t.Fatalf("mkdirAllSync on existing dir: %v", err)
	}

	leaf := filepath.Join(base, "nested", "deep")
	if err := mkdirAllSync(leaf); err != nil {
		t.Fatalf("mkdirAllSync on missing multi-level dir: %v", err)
	}
	if fi, err := os.Stat(leaf); err != nil || !fi.IsDir() {
		t.Fatalf("mkdirAllSync did not create leaf: stat=%v err=%v", fi, err)
	}

	asFile := filepath.Join(base, "afile")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllSync(asFile); err == nil {
		t.Fatal("mkdirAllSync over a file: want error, got nil")
	}
}

// TestSessionDirIsAlwaysAbsolute: even with HOME unset, the session dir must
// not silently yield a relative path whose meaning changes with cwd
// (gh#112 / loto-3axo).
func TestSessionDirIsAlwaysAbsolute(t *testing.T) {
	clearIdentityEnv(t)
	if !filepath.IsAbs(sessionDir()) {
		t.Fatalf("sessionDir() not absolute: %q", sessionDir())
	}
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	if !filepath.IsAbs(sessionDir()) {
		t.Fatalf("sessionDir() relative with HOME unset: %q", sessionDir())
	}
}

func TestSessionPathRejectsTraversal(t *testing.T) {
	for _, bad := range []string{"", "..", "../x", "a/b", `a\b`, "x/../y"} {
		if _, err := sessionPath(bad); err == nil {
			t.Errorf("sessionPath(%q): want error", bad)
		}
	}
	if _, err := sessionPath("ok-1"); err != nil {
		t.Errorf("sessionPath(ok-1): %v", err)
	}
}
