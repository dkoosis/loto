package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// enforcement-design.md §10.3: the seven conditions under which the
// enforcement layer can be silently absent, so an uninstalled hook is a
// finding rather than a discovery after an incident. Rules 1 and 2 (global
// core.hooksPath ownership, the reference-transaction hook's reachability)
// are already the guard-reachability table above — extended with a third row
// rather than forked. This file carries rules 3 through 7 and the nested-
// worktree advisory §11 names as the one thing the deleted command-string
// parser is not replaced with (detection, not prevention).

// enforcementFinding is one health check's verdict: ok with a one-line detail,
// or red with a reason and a bash fix block (design.md: a fix block under
// every actionable finding). Independent of the guardStatus table above —
// these checks have no shared cascade, so inducing one condition never
// flips a neighbor's row.
type enforcementFinding struct {
	ok     bool
	label  string
	detail string
	fix    []string
}

// renderEnforcementLegs runs and prints doctor's leg for each of §10.3's
// rules 3 through 7, in that order, plus the §11 nested-worktree advisory.
//
// call_retention (rule 6) is evaluated BEFORE hook_selftest (rule 5) and
// deliberately so: hookPre's own first action is the post_missing sweep
// (cmd_hook.go), so running the self-test first would sweep away the exact
// staleness rule 6 exists to catch before this run ever reports it.
func renderEnforcementLegs(ctx context.Context, rt *runtime, stdout io.Writer) {
	now := time.Now()
	tReport := hookTReport()

	findings := []enforcementFinding{
		checkStalePostMissing(rt, now, tReport),
		checkHookSelfTest(ctx, rt),
		checkTreeHookSettings(rt.RepoTop),
		checkHookBinaryIdentity(),
		checkSharedPreCommitGuard(ctx, rt.RepoTop),
	}
	for _, f := range findings {
		renderEnforcementFinding(stdout, f)
	}
	renderNestedWorktrees(stdout, checkNestedWorktrees(ctx, rt.RepoTop))
}

func renderEnforcementFinding(stdout io.Writer, f enforcementFinding) {
	if f.ok {
		fmt.Fprintf(stdout, "✓ %s %s\n", f.label, f.detail)
		return
	}
	fmt.Fprintf(stdout, "✗ %s %s\n", f.label, f.detail)
	if len(f.fix) == 0 {
		return
	}
	fmt.Fprintln(stdout, "```bash")
	for _, line := range f.fix {
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintln(stdout, "```")
}

// --- rule 3: tree-hook settings entries -------------------------------------

// treeHookEvent names one harness hook slot the tree hooks must occupy, and
// the `loto hook <verb>` subcommand its command line must invoke.
type treeHookEvent struct {
	name string
	verb string
}

// eventPostToolUseFailure is named once: PreToolUse and PostToolUse have no
// third occurrence elsewhere in this package, but this event name also
// appears in cmd_hook.go's usage text and its own help-contract test.
const eventPostToolUseFailure = "PostToolUseFailure"

// treeHookEvents is enforcement-design.md §11's one-line-shim-per-event list:
// PreToolUse feeds the pre hook, PostToolUse and PostToolUseFailure both feed
// post (a command that writes and then exits nonzero is diffed like one that
// succeeded — cmd_hook.go's own usage text).
var treeHookEvents = []treeHookEvent{
	{name: "PreToolUse", verb: hookSubPre},
	{name: "PostToolUse", verb: hookSubPost},
	{name: eventPostToolUseFailure, verb: hookSubPost},
}

// TreeHookMatcher is the exact matcher decided for the tree-hook shim: every
// tool except the three the harness cannot use to write a file (§4's Rules,
// "their matcher excludes anything beyond Read|Grep|Glob"). Decided
// 2026-09-09, loto-ea8y.7, no prior art: this repo's own settings already run
// hook matchers as regex ("Edit|Write" in .claude/settings.json), and a
// negative lookahead reads as "not exactly Read, Grep or Glob" without an
// exhaustive positive tool list a new harness tool would silently fall
// outside of. Exported so loto-ea8y.9 (the sdlc bead that registers the
// shim) imports this constant rather than retyping the string.
//
// Researched at PR #344 review (dk asked whether a documented non-lookahead
// negation form exists): Claude Code's matcher is evaluated as a JavaScript
// regex (RegExp.prototype.test, code.claude.com/docs/en/hooks), which DOES
// support negative lookahead — unlike Go's RE2, which is why this leg
// compares the configured string verbatim rather than evaluating it as a
// pattern. The docs describe only positive forms (exact-name alternation,
// plain regex) and document no exclusion/negation operator, so there is no
// alternative form to also accept — an exhaustive positive alternation of
// every OTHER tool name is the only documented substitute, and it silently
// misses any tool added after the string is written. Equality against this
// one string stands.
const TreeHookMatcher = `^(?!Read$|Grep$|Glob$).*$`

type settingsFile struct {
	Hooks map[string][]settingsMatcherGroup `json:"hooks"`
}

type settingsMatcherGroup struct {
	Matcher string            `json:"matcher"`
	Hooks   []settingsCommand `json:"hooks"`
}

type settingsCommand struct {
	Type    string `json:"type"`
	Command string `json:"command"`
}

// settingsFilesToRead lists the settings files Claude Code actually loads for
// hook registration, in load order: the user's global settings, then every
// settings*.json the repo carries (settings.json, settings.local.json). A
// plugin's own hooks.json is deliberately not read here — it is not one of
// "the settings files Claude Code actually loads" for this purpose, and
// today it still carries the pre-redesign gate-file-lock.sh wiring.
func settingsFilesToRead(repoTop string) []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		out = append(out, filepath.Join(home, ".claude", "settings.json"))
	}
	if repoTop != "" {
		matches, _ := filepath.Glob(filepath.Join(repoTop, ".claude", "settings*.json"))
		sort.Strings(matches)
		out = append(out, matches...)
	}
	return out
}

// treeHookPresent reports whether any settings file registers event with the
// exact TreeHookMatcher and a command invoking `loto hook <verb>`. A missing
// file, unreadable JSON, or an absent hooks key all read as "not present" —
// diagnostic input, not an error this leg can act on.
func treeHookPresent(files []string, event treeHookEvent) bool {
	want := "loto hook " + event.verb
	for _, path := range files {
		sf, ok := readSettingsFile(path)
		if !ok {
			continue
		}
		if groupsInvokeCommand(sf.Hooks[event.name], want) {
			return true
		}
	}
	return false
}

// readSettingsFile reads and decodes one settings file, folding "missing" and
// "unreadable JSON" into the same false — both are "not present" to the
// caller, not an error this leg can act on.
func readSettingsFile(path string) (settingsFile, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return settingsFile{}, false
	}
	var sf settingsFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return settingsFile{}, false
	}
	return sf, true
}

// groupsInvokeCommand reports whether any matcher group in groups carries the
// exact TreeHookMatcher and a command containing want.
func groupsInvokeCommand(groups []settingsMatcherGroup, want string) bool {
	for _, group := range groups {
		if group.Matcher != TreeHookMatcher {
			continue
		}
		for _, cmd := range group.Hooks {
			if strings.Contains(cmd.Command, want) {
				return true
			}
		}
	}
	return false
}

// checkTreeHookSettings is rule 3. Absence is the expected state until
// loto-ea8y.9 registers the shim — that red is correct, not a bug in this
// leg.
func checkTreeHookSettings(repoTop string) enforcementFinding {
	var missing []string
	files := settingsFilesToRead(repoTop)
	for _, ev := range treeHookEvents {
		if !treeHookPresent(files, ev) {
			missing = append(missing, ev.name)
		}
	}
	if len(missing) == 0 {
		return enforcementFinding{ok: true, label: "tree_hooks", detail: "registered events=3"}
	}
	return enforcementFinding{
		label:  "tree_hooks",
		detail: "missing=" + strings.Join(missing, ","),
		fix:    treeHookSettingsHunk(),
	}
}

// treeHookSettingsHunk renders the exact "hooks" stanza this leg wants
// registered, so the ✗ row hands the registering session (loto-ea8y.9)
// something to copy verbatim rather than reconstruct from a prose summary
// (PR #344 review). Built from treeHookEvents and TreeHookMatcher directly —
// one source, so this hunk cannot drift from what treeHookPresent actually
// checks. encoding/json sorts map keys, so the three events print in the
// same order every run (design.md: byte-identical output).
func treeHookSettingsHunk() []string {
	groups := make(map[string][]settingsMatcherGroup, len(treeHookEvents))
	for _, ev := range treeHookEvents {
		groups[ev.name] = []settingsMatcherGroup{{
			Matcher: TreeHookMatcher,
			Hooks:   []settingsCommand{{Type: "command", Command: "loto hook " + ev.verb}},
		}}
	}
	b, err := json.MarshalIndent(settingsFile{Hooks: groups}, "", "  ")
	if err != nil {
		// Unreachable for this fixed, always-marshalable shape; naming the
		// matcher plainly beats a panic over a diagnostic leg.
		return []string{"# matcher: " + TreeHookMatcher}
	}
	out := make([]string, 0, bytes.Count(b, []byte("\n"))+2)
	out = append(out, `# merge this "hooks" stanza into ~/.claude/settings.json (or the repo's .claude/settings.json):`)
	for line := range strings.SplitSeq(string(b), "\n") {
		out = append(out, "# "+line)
	}
	return out
}

// --- rule 4: installed hook target's hash ----------------------------------

// lookPathLoto and runningExecutable are test seams for checkHookBinaryIdentity
// — same pattern as hookStdin (cmd_hook.go): a test cannot make exec.LookPath
// or os.Executable resolve to a controlled fixture pair otherwise, since the
// test binary itself is what os.Executable reports mid-test-run.
var (
	lookPathLoto      = func() (string, error) { return exec.LookPath("loto") } //nolint:gochecknoglobals // test seam
	runningExecutable = os.Executable                                           //nolint:gochecknoglobals // test seam
)

// checkHookBinaryIdentity is rule 4: the binary a hook script actually
// invokes (bare `loto` on PATH — .githooks/hooks.d/reference-transaction/
// 10-loto-ref-guard's own `command -v loto` shape) must be byte-identical to
// the binary running this doctor command. A stale PATH entry left behind by
// an old install would otherwise run silently forever; git-commit staleness
// (renderBinaryIdentity above) cannot see this because it compares this
// process's own build stamp to repo HEAD, not to a second file on disk.
func checkHookBinaryIdentity() enforcementFinding {
	const label = "hook_binary"
	running, err := runningExecutable()
	if err != nil {
		return enforcementFinding{label: label, detail: "reason=running-binary-unknown detail=" + err.Error()}
	}
	installed, err := lookPathLoto()
	if err != nil {
		return enforcementFinding{
			label:  label,
			detail: "reason=not-on-path",
			fix:    []string{"make install"},
		}
	}
	runningHash, err := hashFile(running)
	if err != nil {
		return enforcementFinding{label: label, detail: "reason=unreadable detail=" + running}
	}
	installedHash, err := hashFile(installed)
	if err != nil {
		return enforcementFinding{label: label, detail: "reason=unreadable detail=" + installed}
	}
	if runningHash == installedHash {
		return enforcementFinding{ok: true, label: label, detail: "match path=" + installed}
	}
	return enforcementFinding{
		label:  label,
		detail: fmt.Sprintf("reason=hash-mismatch installed=%s running=%s", installed, running),
		fix:    []string{"make install"},
	}
}

// hashFile returns the sha256 of a file's content, hex-encoded.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// --- rule 5: synthetic pre-then-post self-test ------------------------------

// selfTestPre and selfTestPost are test seams onto hookPre/hookPost — same
// pattern as hookStdin (cmd_hook.go): a test cannot otherwise force this
// leg's own red condition ("writes no record pair") without corrupting the
// store for every other leg in the same run.
var (
	selfTestPre  = hookPre  //nolint:gochecknoglobals // test seam
	selfTestPost = hookPost //nolint:gochecknoglobals // test seam
)

// checkHookSelfTest is rule 5: a synthetic pre then post fed to the same
// `loto hook` machinery the harness drives must write exactly one call
// record carrying both halves. It runs hookPre/hookPost directly against
// doctor's own open runtime rather than shelling out to `loto hook` — same
// store, no extra process, and doctor's own identity is already pinned.
//
// The tool name is deliberately not Edit-family (isHookEditFamily) and no
// file_path is set, so this call never attempts I2 admission and never takes
// a lock as a side effect of running doctor.
//
// The record and its hook_timing events are deleted synchronously before
// this leg returns (PR #344 review, CI run 34360434734): ReadEnforcementStats
// (loto-ea8y.8) counts both, added after this leg first shipped, so a row left
// for store.DropFinishedCalls' own retention window moved doctor's stats row
// on the very next run and broke byte-identical output. store.DropFinishedCalls'
// comment still names "doctor's self-test" as a reader a finished record must
// survive FOR — this leg is that reader, and it has already read the pair via
// CallRecord below by the time it deletes it.
func checkHookSelfTest(ctx context.Context, rt *runtime) enforcementFinding {
	const label = "hook_selftest"
	callID := "loto-doctor-selftest-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	ev := hookEvent{ToolName: "DoctorSelfTest", ToolUseID: callID, CWD: rt.RepoTop}
	fix := []string{"ls -la " + shellQuote(rt.StateDir) + "   # confirm the state dir is writable"}

	finding := runHookSelfTestOnce(ctx, rt, ev, callID, label, fix)

	// Cleanup runs for every outcome above — a pre-refusal writes no row, a
	// post-refusal leaves a pre-only row, success leaves a full pair — so
	// this leg is a no-op on the store on every return path, pass or fail.
	if _, _, err := rt.Store.DeleteCallAndTiming(rt.Ctx, callID); err != nil && finding.ok {
		return enforcementFinding{label: label, detail: "reason=cleanup-failed detail=" + err.Error(), fix: fix}
	}
	return finding
}

// runHookSelfTestOnce runs the synthetic pre-then-post pair and reports the
// verdict, without touching what it wrote — checkHookSelfTest above owns
// cleanup, so every return path here (including the early ones) still leaves
// a full or partial record for it to delete.
func runHookSelfTestOnce(ctx context.Context, rt *runtime, ev hookEvent, callID, label string, fix []string) enforcementFinding {
	var discard bytes.Buffer
	if code := selfTestPre(ctx, rt, ev, time.Now(), &discard, &discard); code != 0 {
		return enforcementFinding{label: label, detail: fmt.Sprintf("reason=pre-refused exit=%d", code), fix: fix}
	}
	if code := selfTestPost(ctx, rt, ev, time.Now(), &discard); code != 0 {
		return enforcementFinding{label: label, detail: fmt.Sprintf("reason=post-refused exit=%d", code), fix: fix}
	}
	call, _, ok, err := rt.Store.CallRecord(rt.Ctx, callID)
	if err != nil || !ok || call.TPost.IsZero() {
		return enforcementFinding{label: label, detail: "reason=no-record-pair", fix: fix}
	}
	return enforcementFinding{ok: true, label: label, detail: "pre+post recorded"}
}

// --- rule 6: post_missing not kept current ----------------------------------

// checkStalePostMissing is rule 6: an in-flight call older than T_report and
// not yet marked post_missing is the enforcement layer's own bookkeeping
// falling behind — the sweep that marks it lives inside hookPre (cmd_hook.go)
// and only runs when a tree hook actually fires. Read-only: this leg reports
// the raw state without running that sweep itself, which is why it must run
// before checkHookSelfTest below (whose own hookPre call performs it as a
// side effect and would otherwise mask what this leg exists to catch).
func checkStalePostMissing(rt *runtime, now time.Time, tReport time.Duration) enforcementFinding {
	const label = "call_retention"
	calls, err := rt.Store.InFlightCalls(rt.Ctx)
	if err != nil {
		return enforcementFinding{label: label, detail: "reason=read-error detail=" + err.Error()}
	}
	var stale []string
	for i := range calls {
		if calls[i].PostMissing {
			continue
		}
		if now.Sub(calls[i].TPre) > tReport {
			stale = append(stale, calls[i].CallID)
		}
	}
	if len(stale) == 0 {
		return enforcementFinding{ok: true, label: label, detail: "no unmarked stale calls"}
	}
	sort.Strings(stale)
	return enforcementFinding{
		label:  label,
		detail: fmt.Sprintf("stale=%d first=%s", len(stale), stale[0]),
		fix:    []string{"loto doctor   # the self-test's pre-hook sweep marks post_missing on the next run"},
	}
}

// --- rule 7 / R8: shared-checkout pre-commit guard reachable ---------------

// sharedGuardMarker is the line the shared-checkout pre-commit guard itself
// carries (~/.config/git/hooks/pre-commit's header, sd-hmqi) so this leg
// names the right file rather than any executable that happens to sit at the
// resolved hooksPath. The command-string parser that used to sit beside that
// guard was deleted 2026-09-09 and is deliberately not looked for here (R8's
// own wording, this bead's Givens).
const sharedGuardMarker = "guard-marker: sdlc pre-commit guard"

// resolveGlobalHooksPath reads core.hooksPath at GLOBAL scope only — no
// --local, no effective-value fallthrough — so this leg is decoupled from
// whatever a repo's own LOCAL override currently points at. ok is false when
// the key is unset at global scope (exit 1, git's own "unset" signal, same
// convention resolveGitHooksPath reads for the effective value above).
func resolveGlobalHooksPath(ctx context.Context, repoTop string) (resolved string, ok bool, err error) {
	raw, cerr := gitCmd(ctx, repoTop, "config", "--global", "--get", "core.hooksPath")
	raw = strings.TrimSpace(raw)
	if cerr != nil {
		var exitErr *exec.ExitError
		if !errors.As(cerr, &exitErr) || exitErr.ExitCode() != 1 {
			return "", false, fmt.Errorf("git config --global --get core.hooksPath: %w", cerr)
		}
		return "", false, nil // exit 1: key unset at global scope, not an error
	}
	if raw == "" {
		return "", false, nil
	}
	// A relative core.hooksPath is resolved against the repo top the same way
	// resolveGitHooksPath does for the effective value — git itself resolves
	// a relative hooksPath (local or global) against the working repo, not
	// against $HOME, so a global value given as a bare or relative path
	// (rare, but valid config) must not read as absent here (Copilot review,
	// PR #344).
	if !filepath.IsAbs(raw) {
		return filepath.Clean(filepath.Join(repoTop, raw)), true, nil
	}
	return filepath.Clean(raw), true, nil
}

// checkSharedPreCommitGuard is rule 7 (R8): the shared-checkout guard is
// git's own pre-commit hook under the GLOBAL core.hooksPath — the Givens'
// own wording, and deliberately the global scope rather than the effective
// (local-overridable) one resolveGitHooksPath above reads for rules 1/2.
// The two are independent config layers: `make hooks` sets LOCAL
// core.hooksPath to loto's own .githooks so rules 1/2 read green, and that
// local override shadows the global value for git's own dispatch without
// erasing it — the global slot sdlc's guard installs into (§2.6: "two repos
// installing guards into one global hooks directory") stays a fact this leg
// can still check on its own, independent of what loto's repo currently
// overrides it with.
func checkSharedPreCommitGuard(ctx context.Context, repoTop string) enforcementFinding {
	const label = "shared_guard"
	global, ok, err := resolveGlobalHooksPath(ctx, repoTop)
	if err != nil {
		return enforcementFinding{label: label, detail: "reason=hooksPath-unreadable detail=" + err.Error()}
	}
	if !ok {
		return enforcementFinding{
			label:  label,
			detail: "reason=global-hooksPath-unset",
			fix:    []string{`bash "${CLAUDE_PLUGIN_ROOT}/scripts/tool-install-hooks.sh"`},
		}
	}
	target := filepath.Join(global, "pre-commit")
	fi, statErr := os.Stat(target)
	if statErr != nil || fi.IsDir() || fi.Mode()&0o111 == 0 {
		return enforcementFinding{
			label:  label,
			detail: "reason=unreachable detail=" + target,
			fix:    []string{`bash "${CLAUDE_PLUGIN_ROOT}/scripts/tool-install-hooks.sh"`},
		}
	}
	b, readErr := os.ReadFile(target)
	if readErr != nil || !bytes.Contains(b, []byte(sharedGuardMarker)) {
		return enforcementFinding{
			label:  label,
			detail: "reason=marker-absent detail=" + target,
			fix:    []string{"# " + target + " exists but is not the sdlc shared-checkout guard"},
		}
	}
	return enforcementFinding{ok: true, label: label, detail: "reachable entry=" + target}
}

// --- §11 advisory: nested worktree -----------------------------------------

// nestedWorktreePattern is spec §11's shape for the one thing the deleted
// command-string parser used to refuse by reading `git worktree add`'s
// command line: a worktree whose own path is nested inside another
// worktree's tree (a relative path passed to `git worktree add` from inside
// a lane, sd-y1wx). I1 sees the new branch ref, not the path — this is
// detection replacing prevention, not a like-for-like refusal.
var nestedWorktreePattern = regexp.MustCompile(`worktrees/.*worktrees/`)

// nestedWorktreeFinding is one nested worktree: its own path, and the
// containing worktree's path when one of the other listed worktrees is
// provably its ancestor directory.
type nestedWorktreeFinding struct {
	Inner string
	Outer string
}

// checkNestedWorktrees runs `git worktree list` and reports every worktree
// path matching nestedWorktreePattern, paired with whichever OTHER listed
// worktree's path is its containing directory.
func checkNestedWorktrees(ctx context.Context, repoTop string) []nestedWorktreeFinding {
	if repoTop == "" {
		return nil
	}
	out, err := gitCmd(ctx, repoTop, "worktree", "list", "--porcelain")
	if err != nil {
		return nil
	}
	paths := parseWorktreePaths(out)
	var findings []nestedWorktreeFinding
	for _, p := range paths {
		if !nestedWorktreePattern.MatchString(filepath.ToSlash(p)) {
			continue
		}
		findings = append(findings, nestedWorktreeFinding{Inner: p, Outer: containingWorktree(paths, p)})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Inner < findings[j].Inner })
	return findings
}

// parseWorktreePaths pulls every "worktree <path>" line out of `git worktree
// list --porcelain` output, in listed order.
func parseWorktreePaths(porcelain string) []string {
	var paths []string
	for line := range strings.SplitSeq(porcelain, "\n") {
		if p, ok := strings.CutPrefix(line, "worktree "); ok {
			paths = append(paths, p)
		}
	}
	return paths
}

// containingWorktree returns whichever OTHER path in paths is candidate's
// deepest containing directory, or "" when none of the other listed
// worktrees is an ancestor of candidate.
func containingWorktree(paths []string, candidate string) string {
	slash := filepath.ToSlash(candidate)
	outer := ""
	for _, q := range paths {
		if q == candidate {
			continue
		}
		qSlash := filepath.ToSlash(q)
		if strings.HasPrefix(slash, qSlash+"/") && len(qSlash) > len(outer) {
			outer = q
		}
	}
	return outer
}

// renderNestedWorktrees prints one ⚠ row per nested worktree, naming both
// paths (design.md: advisory, not a red finding — §11's replacement for the
// deleted refusal is detection).
func renderNestedWorktrees(stdout io.Writer, findings []nestedWorktreeFinding) {
	for _, f := range findings {
		outer := f.Outer
		if outer == "" {
			outer = "no-containing-worktree-listed"
		}
		fmt.Fprintf(stdout, "⚠ nested_worktree inner=%s outer=%s\n", relPath(f.Inner), relPath(outer))
		fmt.Fprintln(stdout, "```bash")
		fmt.Fprintf(stdout, "git worktree remove %s\n", shellQuote(f.Inner))
		fmt.Fprintln(stdout, "```")
	}
}
