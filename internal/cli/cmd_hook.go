package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/gate"
	"loto/internal/identity"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("hook", cmdHook) } //nolint:gochecknoinits // command registry pattern

const hookUsageHead = `usage: loto hook <pre|post|ref>

Hook bodies. Not run by hand: pre|post are wired as harness hooks in settings
and read the tool-call event JSON on stdin; ref is wired as a chain entry under
.githooks/hooks.d/reference-transaction/ and reads git's transaction on stdin.

pre|post record what the tree looked like around one harness tool call: every
locked path and every path 'git status --porcelain' lists, with its digest
before and after, and a per-path transition sequence so a later reader can
order changes without trusting a clock. PostToolUse and PostToolUseFailure both
feed 'post' — a command that writes and then exits nonzero is diffed like one
that succeeded.

Neither reads a command string. pre|post decode into a struct with no field for
one; ref is handed no operation name by git and decides on (old, new, ref) and
the live-session set alone (enforcement-design.md §10 tooth 1, §5 I1).

  pre   admits an Edit-family write — admitted iff the path is unlocked or
        already yours, and an unlocked path is taken — then records the
        before-state. A refused write exits 2 with the holder named.
  post  records the after-state and closes the call, giving each path whose
        state changed the next number in its sequence. Repeating a post for
        one tool_use_id changes nothing.
  ref <phase>
        reference-transaction body (I1). At the 'prepared' phase, with two or
        more sessions live in this checkout, refuses exactly three shapes:
        HEAD changing its symbolic target (a branch switch), any update to
        refs/stash, and a branch deletion under refs/heads/. A branch tip
        moving, refs/remotes pruning, pack-refs and reflog expiry all pass, as
        does re-attaching a detached HEAD where it already sits. Override by
        taking the checkout-wide claim.

exit: 0 recorded (or nothing this hook can act on, or the ref is allowed)
      1 the ref transition is refused — git aborts the transaction
      2 the write is refused, or usage
      3 ref could not read what it needed and failed open

examples:
  loto hook pre  < event.json
  loto hook post < event.json
  loto hook ref prepared < transaction.txt
`

// hookTReportDefault is the window after which a call with no post is marked
// post_missing (enforcement-design.md §3). It changes what is REPORTED and
// nothing about contention: the call stays in flight, because a command that
// has not posted may still be running and may write next.
const hookTReportDefault = 10 * time.Minute

// hookTReportEnv overrides that window. An env var rather than a config file
// because loto has no config file and LOTO_BASE / LOTO_GATE already establish
// the environment as where this repo's knobs live.
const hookTReportEnv = "LOTO_T_REPORT"

// hookAdmitIntent is stamped on a lock the pre-hook takes on the caller's
// behalf, so `loto status` and a blocked peer can say where the row came from.
// I2's admission is not a lease anybody asked for, so the verb supplies the
// reason the way `loto beacon` does.
const hookAdmitIntent = "hook: admitted Edit-family write"

// hookStdin is the harness event source, a seam so the tests can drive a
// synthetic pre-then-post pair without a subprocess. Same pattern as
// commitTxFn in the store.
var hookStdin io.Reader = os.Stdin //nolint:gochecknoglobals // test seam for the stdin-fed hook

// hookEvent is the Claude Code PreToolUse / PostToolUse / PostToolUseFailure
// payload, in the ONE shape loto is allowed to read.
//
// ‡ The absence below is the mechanism, not an omission. encoding/json
// discards a field this struct does not declare, so the tool input's command
// string is never materialized in this process — enforcement-design.md §10's
// first tooth holds by construction, not by a reviewer remembering to check.
// Adding a field for it would be the whole of the regression this design
// deletes. TestNoHookReadsTheCommandString is what keeps it absent.
type hookEvent struct {
	SessionID string        `json:"session_id"`
	ToolName  string        `json:"tool_name"`
	ToolUseID string        `json:"tool_use_id"`
	CWD       string        `json:"cwd"`
	ToolInput hookToolInput `json:"tool_input"`
}

// hookToolInput carries exactly one field, and the reason it is safe to read
// is that it is structured: the harness put a path there, no parser guessed it
// out of a command line.
type hookToolInput struct {
	FilePath string `json:"file_path"`
}

// hookEditFamily are the tools whose write target the harness states outright,
// and therefore the only ones I2 admission applies to (§4, fourth row).
// Read|Grep|Glob are excluded by the settings matcher, not here: an allowlist
// of tools that cannot write belongs where the hook is wired, so a tool loto
// has never heard of still gets observed.
func isHookEditFamily(tool string) bool {
	switch tool {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		return true
	}
	return false
}

func cmdHook(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "pre":
		return runHook(ctx, true, stdout, stderr)
	case "post":
		return runHook(ctx, false, stdout, stderr)
	case "ref":
		return cmdHookRef(ctx, args[1:], stdout, stderr)
	case "-h", flagHelpLong, subHelp:
		fmt.Fprint(stdout, hookUsageHead)
		return 0
	default:
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}
}

// hookSkip is every non-refusal exit: the hook could not act, said so, and got
// out of the way with 0.
//
// ‡ Deliberate, and the one place this command departs from the CLI's exit-3
// infra convention. This code runs on every tool call of every session. A hook
// that returns nonzero on an unreadable event, an unpinned identity or a store
// that will not open turns one broken precondition into a session that cannot
// run any tool at all — the outage the gate is forbidden to become. The
// warning is on stderr, where the harness surfaces it, so the failure is
// visible rather than silent.
func hookSkip(stderr io.Writer, format string, a ...any) int {
	fmt.Fprintf(stderr, "⚠ hook: "+format+"\n", a...)
	return 0
}

func runHook(ctx context.Context, pre bool, stdout, stderr io.Writer) int {
	started := time.Now()
	var ev hookEvent
	if err := json.NewDecoder(hookStdin).Decode(&ev); err != nil {
		return hookSkip(stderr, "unreadable event on stdin: %v", err)
	}
	if ev.ToolUseID == "" {
		return hookSkip(stderr, "event carries no tool_use_id; a call record has nothing to be keyed to")
	}
	// Checked before the runtime opens: an unpinned caller gets a throwaway
	// owner from identity.Ephemeral, and a call record owned by a uuid that
	// exists for one process is worse than no record (loto-pody).
	if !identity.PinnedByEnv() {
		return hookSkip(stderr, "%v", errIdentityUnpinned)
	}
	// openRuntime, not openRuntimeGC: this is the per-tool-call hot path and
	// must not pay the session GC sweep (loto-6pn6, same reason `loto beacon`
	// takes this door).
	rt, err := openRuntime(ctx)
	if err != nil {
		return hookSkip(stderr, "%v", err)
	}
	defer rt.Close()

	if pre {
		return hookPre(ctx, rt, ev, started, stdout, stderr)
	}
	return hookPost(ctx, rt, ev, started, stderr)
}

// hookPre is I3 steps 3 and 4. Step 1's verdicts and step 2's drift report are
// the next bead (loto-ea8y.6); the sweep below is the slot they will occupy,
// and it does the three things that must happen whether or not verdicts exist:
// end the calls whose owner is gone, flag the calls past T_report, and drop the
// finished records nothing can still contest.
func hookPre(ctx context.Context, rt *runtime, ev hookEvent, started time.Time, stdout, stderr io.Writer) int {
	now := time.Now()
	tReport := hookTReport()
	// Dead-owner first: it is what MOVES a call out of flight, and the two
	// sweeps after it both read the in-flight set.
	if _, err := rt.Store.MarkDeadOwnerCalls(rt.Ctx, now, hookSessionIsDead); err != nil {
		fmt.Fprintf(stderr, "⚠ hook: dead-owner sweep: %v\n", err)
	}
	if _, err := rt.Store.MarkPostMissing(rt.Ctx, now, tReport); err != nil {
		fmt.Fprintf(stderr, "⚠ hook: post_missing sweep: %v\n", err)
	}
	if _, err := rt.Store.DropFinishedCalls(rt.Ctx, now, tReport); err != nil {
		fmt.Fprintf(stderr, "⚠ hook: call-record retention: %v\n", err)
	}

	// Step 3 — admission, BEFORE the observation, so a path this call just
	// took reads as its own at pre (§5 I2).
	declared := ""
	if isHookEditFamily(ev.ToolName) && ev.ToolInput.FilePath != "" {
		var blocker *domain.LockRecord
		declared, blocker = hookAdmit(ctx, rt, ev.ToolInput.FilePath)
		if blocker != nil {
			render.EmitHookRefusal(stderr, blocker.Target.Canonical,
				string(blocker.OwnerUUID), blocker.Intent, blocker.ExpiresAt)
			return 2
		}
	}

	// Step 4 — record.
	obs, statusPaths, lockedBytes, err := hookObserve(ctx, rt, declared, stderr)
	if err != nil {
		return hookSkip(stderr, "observe: %v", err)
	}
	recorded, err := rt.Store.RecordCallPre(rt.Ctx, store.HookCall{
		CallID:      ev.ToolUseID,
		OwnerUUID:   domain.AgentUUID(rt.Agent.UUID),
		SessionUUID: rt.SessionUUID,
		ToolName:    ev.ToolName,
		TPre:        now,
	}, obs)
	if err != nil {
		return hookSkip(stderr, "record pre: %v", err)
	}
	if !recorded {
		return 0 // a repeated pre; the first observation stands
	}
	hookEmitTiming(rt, ev.ToolUseID, "pre", time.Since(started), statusPaths, lockedBytes, stderr)
	fmt.Fprintf(stdout, "✓ recorded call=%s phase=pre paths=%d\n", ev.ToolUseID, len(obs))
	return 0
}

// hookPost is I4 step 5's record half. Step 6's verdicts are the next bead.
func hookPost(ctx context.Context, rt *runtime, ev hookEvent, started time.Time, stderr io.Writer) int {
	obs, statusPaths, lockedBytes, err := hookObserve(ctx, rt, "", stderr)
	if err != nil {
		return hookSkip(stderr, "observe: %v", err)
	}
	// The post observation set must cover every path the pre recorded, even
	// one that is clean and unlocked now — that IS the state change §10a test
	// 16 asks for, and a path missing from the set would read as unchanged.
	obs, err = hookAddRecordedPaths(rt, ev.ToolUseID, obs, stderr)
	if err != nil {
		return hookSkip(stderr, "read call record: %v", err)
	}
	out, err := rt.Store.RecordCallPost(rt.Ctx, ev.ToolUseID, time.Now(), obs)
	if err != nil {
		return hookSkip(stderr, "record post: %v", err)
	}
	if !out.Recorded {
		return 0 // repeat post, or a call this checkout never opened
	}
	hookEmitTiming(rt, ev.ToolUseID, "post", time.Since(started), statusPaths, lockedBytes, stderr)
	return 0
}

// hookSessionIsDead is the liveness oracle the dead-owner sweep asks. It is
// the same session-record probe runtime.liveProbe consults, so a call and a
// lock held by one session can never be judged by two different rules.
//
// ‡ Only SessionDead ends a call. SessionUnknown — no record on disk, a peer
// on another host, a record this process cannot read — leaves it in flight.
// That is loto's standing asymmetry: a false "gone" hands a live agent's
// territory away, a false "alive" only delays a reclaim.
func hookSessionIsDead(s domain.SessionUUID) bool {
	if s == "" {
		return false
	}
	return identity.ProbeSession(string(s)).Liveness == identity.SessionDead
}

// hookAddRecordedPaths widens the post observation set with every path the pre
// recorded that the post did not re-observe — a locked file that went clean, a
// dirty path restored to HEAD. Each is re-read here so the post carries its
// real current state rather than inheriting the pre digest by omission.
func hookAddRecordedPaths(rt *runtime, callID string, obs []store.HookPathState, warn io.Writer) ([]store.HookPathState, error) {
	_, recorded, ok, err := rt.Store.CallRecord(rt.Ctx, callID)
	if err != nil || !ok {
		return obs, err
	}
	have := make(map[string]bool, len(obs))
	for i := range obs {
		have[obs[i].Path] = true
	}
	var missing []string
	byPath := map[string]store.HookCallPath{}
	for i := range recorded {
		if have[recorded[i].Path] {
			continue
		}
		missing = append(missing, recorded[i].Path)
		byPath[recorded[i].Path] = recorded[i]
	}
	if len(missing) == 0 {
		return obs, nil
	}
	states := hookReadPaths(rt.Ctx, rt.RepoTop, missing, warn)
	for i := range states {
		p := byPath[states[i].Path]
		states[i].Locked = p.Locked
		states[i].Declared = p.Declared
		states[i].Epoch = p.Epoch
		states[i].Holder = p.Holder
		obs = append(obs, states[i])
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].Path < obs[j].Path })
	return obs, nil
}

// hookAdmit is I2. It returns the declared path when the write is admitted,
// or the live foreign lock that refuses it.
//
// A path that does not resolve into this repo is neither admitted nor refused:
// the tree hooks observe the checkout the session runs in, and a write outside
// it is out of frame (§11, "one more loss, named").
func hookAdmit(ctx context.Context, rt *runtime, filePath string) (declared string, blocker *domain.LockRecord) {
	// resolveGitTarget, not resolveCLITarget: the harness put this path in a
	// structured field, so no shell ever touched it and the shell-token
	// spelling rule would only reject legitimate names (domain.Provenance).
	t, err := resolveGitTarget(nil, callerBase(), rt.RepoTop, filePath)
	if err != nil {
		return "", nil
	}
	// allowMissing: a Write that CREATES the file is the case I2 most needs
	// to admit (loto-z5nb, the same reason `loto beacon` tolerates ENOENT).
	if reason := statFileTargetReason(rt.RepoTop, t.Canonical, true); reason != "" {
		return "", nil
	}
	rows, err := rt.Store.LocksAt(rt.Ctx, t)
	if err != nil {
		return "", nil
	}
	kin, err := parentKin(ctx)
	if err != nil {
		return "", nil
	}
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), Kin: kin, CaseFold: rt.CaseFold}
	me := domain.AgentUUID(rt.Agent.UUID)
	// L(f) = s. A row of the caller's own — or its parent's, which a stamped
	// subagent's Bash-side `loto lock` wrote — is authorization already held,
	// and re-deciding it here would refuse a worker on territory it just took
	// (the loto-fs84 hole, one layer up).
	for i := range rows {
		if rows[i].OwnerUUID == me || ec.IsKin(rows[i].OwnerUUID) {
			return t.Canonical, nil
		}
	}
	// L(f) = s' ≠ s. Any live foreign row refuses, beacon or lease alike: a
	// beacon means an agent is writing here right now, which is the case I2
	// exists to serialize.
	for i := range rows {
		if ec.IsStale(rows[i]) {
			continue
		}
		return "", &rows[i]
	}
	// L(f) = ⊥. Take it, and let the store's compare-and-set be the arbiter of
	// the race between this read and the write.
	return hookTakeLock(rt, t, me, kin, ec)
}

// hookTakeLock is I2's `L(f) := s` half: the lock the pre-hook takes on the
// caller's behalf when the path is unlocked. A lost race re-reads rather than
// guessing, so a refusal names the owner who actually holds the path now.
func hookTakeLock(rt *runtime, t domain.Target, me domain.AgentUUID, kin []domain.AgentUUID, ec domain.EvalContext) (string, *domain.LockRecord) {
	now := time.Now()
	rec := domain.LockRecord{
		Target:      t,
		OwnerUUID:   me,
		SessionUUID: rt.SessionUUID,
		Intent:      hookAdmitIntent,
		CreatedAt:   now,
		ExpiresAt:   now.Add(domain.DegradedModeTTL),
		Host:        rt.Host,
		Mode:        domain.ModeExclusive,
	}
	if _, err := rt.Store.AcquireLocks(rt.Ctx, []domain.LockRecord{rec}, memoLiveProbe(rt.liveProbe()), kin...); err != nil {
		held, lerr := rt.Store.LocksAt(rt.Ctx, t)
		if lerr != nil {
			return "", nil
		}
		for i := range held {
			if held[i].OwnerUUID != me && !ec.IsStale(held[i]) {
				return "", &held[i]
			}
		}
		return "", nil
	}
	return t.Canonical, nil
}

// hookObserve reads the state §5 I3 step 4 records: every path with a live
// lock, own and peers' alike, plus every path `git status --porcelain` lists.
// declared, when non-empty, is the Edit-family path this call was admitted on.
//
// Returns the observations sorted by path (same input, byte-identical record),
// how many paths git status listed, and the total bytes under lock — the two
// numbers the timing counter reports.
func hookObserve(ctx context.Context, rt *runtime, declared string, warn io.Writer) (obs []store.HookPathState, statusPaths int, lockedBytes int64, err error) {
	locks, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		return nil, 0, 0, err
	}
	ec := domain.EvalContext{Now: time.Now(), Live: memoLiveProbe(rt.liveProbe()), CaseFold: rt.CaseFold}
	holders := hookLiveHolders(locks, ec)

	status, err := gitStatusPaths(ctx, rt.RepoTop)
	if err != nil {
		return nil, 0, 0, err
	}
	statusPaths = len(status)

	set := map[string]bool{}
	for p := range holders {
		set[p] = true
	}
	for _, p := range status {
		set[p] = true
	}
	if declared != "" {
		set[declared] = true
	}
	paths := make([]string, 0, len(set))
	for p := range set {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	// seq(f, E) is read HERE — before the first stat, before the first digest,
	// and well before the record transaction that reads it again. The pair
	// (seq, bytes) has to be taken at one moment or the spanning test can
	// exonerate this call from a transition it really covers; RecordCallPre
	// keeps whichever number is lower. See store.HookPathState.SeqAtObserve.
	seqAt, err := hookSeqAtObserve(rt, paths, holders)
	if err != nil {
		return nil, 0, 0, err
	}

	obs = hookReadPaths(ctx, rt.RepoTop, paths, warn)
	for i := range obs {
		h, locked := holders[obs[i].Path]
		obs[i].Locked = locked
		obs[i].Declared = obs[i].Path == declared
		obs[i].SeqAtObserve = seqAt[obs[i].Path]
		obs[i].SeqAtObserveKnown = true
		if locked {
			obs[i].Holder = h.OwnerUUID
			obs[i].Epoch = h.Epoch
			if fi, serr := os.Lstat(filepath.Join(rt.RepoTop, obs[i].Path)); serr == nil {
				lockedBytes += fi.Size()
			}
		}
	}
	return obs, statusPaths, lockedBytes, nil
}

// hookSeqAtObserve reads seq(f, E) for every path about to be observed, so the
// number and the bytes beside it are taken at one moment. Split out of
// hookObserve because it is the whole of the fix and reads as one idea.
func hookSeqAtObserve(rt *runtime, paths []string, holders map[string]domain.LockRecord) (map[string]int64, error) {
	seqAt := make(map[string]int64, len(paths))
	for _, p := range paths {
		var epoch int64
		if h, ok := holders[p]; ok {
			epoch = h.Epoch
		}
		n, err := rt.Store.PathSeq(rt.Ctx, p, epoch)
		if err != nil {
			return nil, err
		}
		seqAt[p] = n
	}
	return seqAt, nil
}

// hookLiveHolders reduces the live lock rows to one holder per path — the `L`
// a call record cites.
//
// ‡ Several live rows can cover one path under shared mode, and `L` in the
// design is single-valued. An exclusive holder is authoritative; among equals
// the lowest owner uuid wins, so the record is deterministic rather than
// whatever order the scan happened to return (.claude/rules/design.md: same
// input, byte-identical output).
func hookLiveHolders(locks []domain.LockRecord, ec domain.EvalContext) map[string]domain.LockRecord {
	holders := map[string]domain.LockRecord{}
	for i := range locks {
		if ec.IsStale(locks[i]) {
			continue
		}
		key := locks[i].Target.Canonical
		cur, seen := holders[key]
		switch {
		case !seen,
			locks[i].EffectiveMode() == domain.ModeExclusive && cur.EffectiveMode() != domain.ModeExclusive,
			locks[i].EffectiveMode() == cur.EffectiveMode() && locks[i].OwnerUUID < cur.OwnerUUID:
			holders[key] = locks[i]
		}
	}
	return holders
}

// hookReadPaths reads stat and digest for each path, sorted by path.
//
// It cannot fail. A path that does not exist gets an empty digest and the
// "absent" stat, and a path that will not hash gets an empty digest and a
// warning — a removal and an odd name are both states, and neither may cost
// the observation of every other path.
func hookReadPaths(ctx context.Context, repoTop string, paths []string, warn io.Writer) []store.HookPathState {
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	present := make([]string, 0, len(sorted))
	stats := make(map[string]string, len(sorted))
	for _, p := range sorted {
		fi, err := os.Lstat(filepath.Join(repoTop, p))
		if err != nil {
			stats[p] = hookStatAbsent
			continue
		}
		stats[p] = hookStatString(fi)
		present = append(present, p)
	}
	digests, err := gate.HashPaths(ctx, repoTop, present)
	if err != nil {
		digests = hookHashEachPath(ctx, repoTop, present, warn)
	}
	out := make([]store.HookPathState, 0, len(sorted))
	for _, p := range sorted {
		out = append(out, store.HookPathState{Path: p, Stat: stats[p], Digest: digests[p]})
	}
	return out
}

// hookHashEachPath is the fallback when the batch hash fails: hash one path at
// a time and skip the ones that will not hash, naming each.
//
// ‡ One unhashable path — a locked file that is now a directory, an unreadable
// mode — used to lose the WHOLE observation, so a single odd path in the tree
// silently stopped every call from being recorded. A path with no digest is
// still recorded, carrying its stat and an empty digest; that reads as changed
// at post, which is the conservative direction.
func hookHashEachPath(ctx context.Context, repoTop string, paths []string, warn io.Writer) map[string]string {
	digests := make(map[string]string, len(paths))
	for _, p := range paths {
		one, err := gate.HashPaths(ctx, repoTop, []string{p})
		if err != nil {
			fmt.Fprintf(warn, "⚠ hook: no digest for %s: %v\n", p, err)
			continue
		}
		digests[p] = one[p]
	}
	return digests
}

// hookStatAbsent is the stat of a path that is not there. A distinct value
// rather than an empty string, so "removed" and "never observed" stay apart.
const hookStatAbsent = "absent"

// hookStatString is `stat(f)`'s cheap part (§3): size, mode, mtime.
//
// ‡ Inode and ctime are deliberately left out. Reading them needs a
// syscall.Stat_t cast and therefore a per-OS file, and the digest — which is
// read for every path anyway — is the authority on whether content changed.
// stat is the corroborator that catches a chmod or a same-content rewrite, and
// mode plus mtime carry that.
func hookStatString(fi os.FileInfo) string {
	return strconv.FormatInt(fi.Size(), 10) + ":" +
		strconv.FormatUint(uint64(fi.Mode().Perm()), 8) + ":" +
		strconv.FormatInt(fi.ModTime().UnixNano(), 10)
}

// gitStatusPaths lists every path `git status --porcelain` reports: modified,
// staged, and untracked. Ignored paths are excluded by git itself, which is
// exactly F's definition (§3).
//
// -z because a path may contain anything but NUL; -uall so an untracked
// DIRECTORY is expanded to the files inside it (the default collapses them to
// the directory, which is not a member of F); --no-renames so every change
// reports as a plain add/modify/delete and no destination path hides inside a
// rename record.
func gitStatusPaths(parent context.Context, repoTop string) ([]string, error) {
	ctx, cancel := context.WithTimeout(parent, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain", "-z", "-uall", "--no-renames")
	cmd.Dir = repoTop
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git status --porcelain: %w", err)
	}
	var paths []string
	for rec := range strings.SplitSeq(string(out), "\x00") {
		// "XY path": two status columns, one space, then the path.
		if len(rec) < 4 {
			continue
		}
		paths = append(paths, rec[3:])
	}
	sort.Strings(paths)
	return paths, nil
}

// hookTReport reads T_report from the environment, falling back to the
// default. A malformed or non-positive value falls back too, rather than
// disabling the sweep on a typo.
func hookTReport() time.Duration {
	raw := os.Getenv(hookTReportEnv)
	if raw == "" {
		return hookTReportDefault
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return hookTReportDefault
	}
	return d
}

// hookTimingDetail is the hook_timing payload (§10b row 3). Exactly the four
// values the cost question reads: which call, how long that half took, how
// many paths git status listed, and how many bytes were under lock.
type hookTimingDetail struct {
	CallID      string `json:"call_id"`
	PreMs       int64  `json:"pre_ms,omitempty"`
	PostMs      int64  `json:"post_ms,omitempty"`
	StatusPaths int    `json:"status_paths"`
	LockedBytes int64  `json:"locked_bytes"`
}

// hookEmitTiming writes the one telemetry row a hook owes. Best-effort like
// every other audit append: a lost breadcrumb costs a measurement, and must
// never undo a call record already on disk.
func hookEmitTiming(rt *runtime, callID, phase string, elapsed time.Duration, statusPaths int, lockedBytes int64, stderr io.Writer) {
	d := hookTimingDetail{CallID: callID, StatusPaths: statusPaths, LockedBytes: lockedBytes}
	if phase == "pre" {
		d.PreMs = elapsed.Milliseconds()
	} else {
		d.PostMs = elapsed.Milliseconds()
	}
	payload, err := json.Marshal(d)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ hook: encode timing: %v\n", err)
		return
	}
	if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
		Kind:      store.EventHookTiming,
		ActorUUID: rt.Agent.UUID,
		Reason:    phase,
		Detail:    string(payload),
		CreatedAt: time.Now(),
	}); err != nil {
		fmt.Fprintf(stderr, "⚠ hook: timing row: %v\n", err)
	}
}
