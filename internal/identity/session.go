package identity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dkoosis/atomicfile"
)

// SessionRecord is what `loto whoami` leaves behind about the Claude Code
// session it ran in, at ~/.loto/session/<sid>.json. It is the evidence
// ProbeSession consults for a holder whose lock pid is gone but whose session
// may still be up — the one presence fact a Go binary cannot get from Claude
// Code itself.
//
// What is derivable inside a SessionStart hook, honestly: the socket and the
// session pid straight from env; the pid's OS start-time from the same reader
// ProbeSession re-reads it with. A field left zero is "unknown", never a
// witness for death.
type SessionRecord struct {
	SessionID string `json:"session_id"`
	// UUID is the owner id the session resolved to (Agent.UUID). GCSessions
	// pins a record whose uuid a live lock row still names.
	UUID   string `json:"uuid"`
	Host   string `json:"host,omitempty"`
	Socket string `json:"socket,omitempty"`
	PID    int    `json:"pid,omitempty"`
	// ProcStart is the OS start-time of the session process, read at record
	// time from the same reader that re-reads it at verdict time. Opaque and
	// host-local (darwin: wall-clock µs; linux: ticks since boot) — only ever
	// compared for EQUALITY against a value read on this host. 0 means
	// unknown: the reader failed, the platform has none, or no pid was known.
	ProcStart int64 `json:"proc_start,omitempty"`
	// Repo is the git toplevel the session started in, as the recording
	// process resolved it. It exists so a liveness question can be asked
	// about ONE checkout: without it, "which owners are live" is a
	// machine-wide answer, and I1 refused a branch switch in this checkout
	// because a session was up in an unrelated repo (loto-ea8y.4 review).
	// Empty means unknown — a record written before this field existed, or a
	// session started outside any repo. Unknown is never counted as "here":
	// I1's failure direction is to pass, and a session re-records on its next
	// `loto whoami`.
	Repo       string    `json:"repo,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// RecordSession writes this process's session record, replacing any earlier
// one for the same session id. Returns (nil, nil) when no session id is in
// the environment — a bare shell has no session to record, which is normal,
// not an error. Publish is by rename, so a concurrent reader never sees a
// half-written record and a re-run from the same session simply refreshes it.
// repoTop is the checkout this session belongs to, resolved by the CALLER:
// identity shells out to nothing, and the cli layer already has the git
// toplevel in hand. "" is a legitimate value — a session outside any repo.
func RecordSession(a *Agent, repoTop string) (*SessionRecord, error) {
	sid := SessionIDFromEnv()
	if sid == "" || a == nil {
		return nil, nil //nolint:nilnil // "no session here" is a normal, non-error outcome
	}
	path, err := sessionPath(sid)
	if err != nil {
		return nil, err
	}
	socket := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET")
	pid := sessionPID(socket)
	procStart, _ := ProcStart(pid) // 0,false → 0 → unknown
	rec := &SessionRecord{
		SessionID:  sid,
		UUID:       a.UUID,
		Host:       a.Host,
		Socket:     socket,
		PID:        pid,
		ProcStart:  procStart,
		Repo:       repoTop,
		RecordedAt: time.Now().UTC(),
	}
	body, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := mkdirAllSync(sessionDir()); err != nil {
		return nil, err
	}
	// atomicfile: sibling temp, fsync, rename over the final path, then fsync
	// the parent dir (loto-cq6 / gh#131), with F_FULLFSYNC on darwin.
	if err := atomicfile.WriteFile(path, body, 0o600); err != nil {
		return nil, err
	}
	return rec, nil
}

// sessionPID resolves the long-lived session process id. LOTO_PID is what the
// SessionStart hook exports and what lock rows are stamped with (cli
// stampPID); CLAUDE_PID is Claude Code's own spelling; the messaging socket's
// basename ("/tmp/cc-socks/63879.sock") carries the same number and covers a
// harness that exports neither. 0 means unknown.
func sessionPID(socket string) int {
	for _, v := range []string{os.Getenv("LOTO_PID"), os.Getenv("CLAUDE_PID")} {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	base := strings.TrimSuffix(filepath.Base(socket), ".sock")
	if n, err := strconv.Atoi(base); err == nil && n > 0 {
		return n
	}
	return 0
}

// readSession loads one session record raw — no pruning side effects. The
// oracle must be a pure observer: a liveness question asked mid-race (session
// restarting, record being rewritten) must not unlink the fresh record.
// ok=false means absent, unreadable, or unparseable.
func readSession(sid string) (SessionRecord, bool) {
	path, err := sessionPath(sid)
	if err != nil {
		return SessionRecord{}, false
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return SessionRecord{}, false
	}
	var rec SessionRecord
	if err := json.Unmarshal(body, &rec); err != nil {
		return SessionRecord{}, false
	}
	return rec, true
}

// LiveOwnerUUIDs answers "which owner uuids have live evidence?" from session
// records alone, with no lock row involved. ProbeSession already answers this
// per-session (keyed on a lock row's SessionUUID field); this reshapes the
// same oracle — readSession + SessionRecord.Verdict, the one liveness rule
// the rest of loto uses — for a caller that starts from an owner uuid with no
// lock to key a probe from, such as a stash's LOTO_AGENT_ID stamp (loto-9spo).
// An owner holding zero locks still ran `loto whoami` at session start and
// left a record behind; this is what makes that record findable without
// going through a lock.
//
// One owner uuid may have several session records (sibling sessions,
// loto-81n): any one of them verdicting Live is enough to count the owner as
// live here — the caller is asking "is this owner doing anything anywhere",
// not "is this specific session up" the way ProbeSession is. A record that
// verdicts SessionUnknown (no witness, or none on disk) contributes nothing:
// unknown is not evidence of life, so an owner with only unknown records is
// left out of the returned set, same as an owner with no record at all — the
// caller's own membership check governs what an absence means.
//
// A ReadDir failure (missing/unreadable session dir) returns nil rather than
// an error, mirroring GCSessions' own non-fatal treatment of the same
// failure — a caller that unions this into another set gets an empty set to
// union against, not a special case to handle.
func LiveOwnerUUIDs() map[string]struct{} { return liveOwnerUUIDs("") }

// LiveOwnerUUIDsInRepo is LiveOwnerUUIDs narrowed to one checkout: only
// records whose Repo is exactly repoTop count.
//
// This is the set I1 is defined over (enforcement-design §3, "live sessions
// in ONE checkout"). The unscoped answer is wrong for that job in a concrete
// way: a session live in any other repo on the machine made this checkout
// refuse a branch switch, a stash and a branch delete with no peer here at
// all (loto-ea8y.4 review). Callers asking the machine-wide question — "is
// this owner doing anything anywhere", which is what doctor's stash triage
// wants — keep LiveOwnerUUIDs.
//
// A record with an empty Repo (written before the field existed, or by a
// session that started outside a repo) is NOT counted: it cannot be shown to
// be here, and I1's failure direction is to pass. Such a session rejoins the
// set on its next `loto whoami`. An empty repoTop returns an empty set for
// the same reason — a caller with no checkout to name has no checkout to
// scope to.
func LiveOwnerUUIDsInRepo(repoTop string) map[string]struct{} {
	if repoTop == "" {
		return map[string]struct{}{}
	}
	return liveOwnerUUIDs(repoTop)
}

// liveOwnerUUIDs is the shared sweep. repoTop == "" means every record,
// whatever checkout it names.
func liveOwnerUUIDs(repoTop string) map[string]struct{} {
	entries, err := os.ReadDir(sessionDir())
	if err != nil {
		return nil
	}
	out := make(map[string]struct{})
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, ok := readSession(strings.TrimSuffix(e.Name(), ".json"))
		if !ok || rec.UUID == "" {
			continue
		}
		if repoTop != "" && rec.Repo != repoTop {
			continue
		}
		if rec.Verdict().Liveness == SessionLive {
			out[rec.UUID] = struct{}{}
		}
	}
	return out
}

// sessionGCMaxAge bounds how long a session record may linger before
// GCSessions reaps it. Measured from the record's mtime, which RecordSession
// refreshes on every `loto whoami`; a session that outlives it without ever
// re-running whoami is implausible at this bound.
const sessionGCMaxAge = 30 * 24 * time.Hour

// sessionGCMaxUnlink bounds one GCSessions reap pass. 12,888 unlinks measured
// 1.81s (loto-6pn6); a neglected box with 500k session files must not turn
// `loto doctor` into a minute-long stall, so the pass stops and reports a
// residual for the next run to finish. Var, not const, so the bound test
// doesn't need to create 5,000+ fixture files.
var sessionGCMaxUnlink = 5000 //nolint:gochecknoglobals // test-tunable knob

// GCSessions removes ~/.loto/session/*.json whose mtime is older than
// sessionGCMaxAge, and reports how many it removed and how many candidates it
// left for a later pass. Three pins, in cost order:
//
//  1. mtime — 30 days exceeds any plausible continuous session by a wide
//     margin.
//  2. keepSID — never reap the caller's own session, whatever its age.
//  3. pinned — owner uuids of live lock rows, passed in by the CLI because
//     identity cannot import store. Scoped to one project's store, so a
//     session holding locks only in another repo rests on pin (1); that is
//     why (1) is generous.
//
// The mtime cutoff is checked before any file is opened, so a steady-state
// pass (nothing stale) parses zero files. Once sessionGCMaxUnlink candidates
// have been reaped this pass, remaining stale entries are counted as residual
// without being parsed or unlinked. Best-effort: a denied read/unlink or a
// racing writer is skipped, not fatal — session hygiene is hygiene, not
// invariant.
func GCSessions(now time.Time, keepSID string, pinned map[string]struct{}) (reaped, residual int, err error) {
	entries, err := os.ReadDir(sessionDir())
	if err != nil {
		return 0, 0, err
	}
	cutoff := now.Add(-sessionGCMaxAge)
	for _, e := range entries {
		if !isStaleSessionCandidate(e, cutoff, keepSID) {
			continue
		}
		// Past the bound, count without paying the read+parse cost — the pin
		// check only matters for a file this pass would otherwise unlink.
		if reaped >= sessionGCMaxUnlink {
			residual++
			continue
		}
		if reapSessionFile(filepath.Join(sessionDir(), e.Name()), pinned) {
			reaped++
		}
	}
	return reaped, residual, nil
}

// sessionGCMarkerName records the last time GCSessionsIfDue actually ran a
// pass, so repeated write-verb calls (openRuntimeGC) can skip the ReadDir
// sweep between runs at the cost of one os.Stat.
const sessionGCMarkerName = ".session-gc-at"

// sessionGCMinInterval bounds how often GCSessionsIfDue runs a real pass.
// Before this, GCSessions only ran from `loto doctor` — a verb dk has to
// remember to invoke — so ~/.loto/session grew unbounded in practice (3,877
// files observed, sd-kx5). Gating on a marker file's mtime keeps the added
// cost on the write-verb hot path to a single stat call in the common case.
var sessionGCMinInterval = 24 * time.Hour //nolint:gochecknoglobals // test-tunable knob

// GCSessionsIfDue runs GCSessions at most once per sessionGCMinInterval,
// tracked by a marker file's mtime under lotoHome(). ran=false means the
// marker was fresh and no sweep happened this call — reaped/residual are
// zero and err is nil. Errors touching the marker are non-fatal.
func GCSessionsIfDue(now time.Time, keepSID string, pinned map[string]struct{}) (ran bool, reaped, residual int, err error) {
	marker := filepath.Join(lotoHome(), sessionGCMarkerName)
	if fi, statErr := os.Stat(marker); statErr == nil {
		if now.Sub(fi.ModTime()) < sessionGCMinInterval {
			return false, 0, 0, nil
		}
	}
	reaped, residual, err = GCSessions(now, keepSID, pinned)
	if mkErr := mkdirAllSync(lotoHome()); mkErr == nil {
		_ = os.WriteFile(marker, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600)
	}
	return true, reaped, residual, err
}

// isStaleSessionCandidate reports whether e is a session record eligible for
// GCSessions to consider reaping: a `.json` file, not the caller's own
// session, and past the mtime cutoff. Checked before any file is opened.
func isStaleSessionCandidate(e os.DirEntry, cutoff time.Time, keepSID string) bool {
	if !strings.HasSuffix(e.Name(), ".json") {
		return false
	}
	if keepSID != "" && e.Name() == keepSID+".json" {
		return false
	}
	info, err := e.Info()
	if err != nil {
		return false
	}
	return info.ModTime().Before(cutoff)
}

// reapSessionFile removes the session record at path unless it names a pinned
// owner uuid, in which case it survives regardless of age. A read, parse, or
// unlink failure is treated as "kept".
func reapSessionFile(path string, pinned map[string]struct{}) bool {
	body, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var ref struct {
		UUID string `json:"uuid"`
	}
	if json.Unmarshal(body, &ref) == nil && ref.UUID != "" {
		if _, keep := pinned[ref.UUID]; keep {
			return false
		}
	}
	return os.Remove(path) == nil
}
