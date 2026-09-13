package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/identity"
	"loto/internal/store"
)

// ── `loto hook ref` — I1, the tree-move refusal ───────────────────────────
//
// One `hook` verb, three bodies: `pre` and `post` are the tree-observation
// halves in cmd_hook.go, and `ref` is this one. The router and the registry
// entry live there, beside the usage text every body shares.
//
// git's `reference-transaction` hook is the one chokepoint every H1 member
// crosses: a branch switch, a stash, a branch deletion and a worktree branch
// all reach it, and a hook exiting non-zero in the `prepared` phase makes git
// abort the whole transaction atomically (enforcement-design §2.4, measured
// on git 2.55.0). Nothing here reads a command string: git hands over no
// operation name, and the decision is on (old, new, ref) and the live-session
// set alone (§5 I1).
//
// The refusal fires only while a peer could be hurt — two or more live
// sessions in this checkout — and the operator's override is a checkout-wide
// claim, `loto claim .`, which is a durable statement of "the whole tree is
// mine right now" rather than an env-var escape nobody can audit.

// The three protected shapes, spelled as the reason a refusal event carries.
const (
	refShapeHeadSymref   = "head-symref"
	refShapeStash        = "stash"
	refShapeBranchDelete = "branch-delete"
)

// refHeadsPrefix / refStash / refHEAD are the ref names I1 is defined over.
const (
	refHeadsPrefix = "refs/heads/"
	refStash       = "refs/stash"
	refHEAD        = "HEAD"
)

// refSymrefPrefix is how git spells a symbolic-ref value in the transaction
// it hands the hook: `0000… ref:refs/heads/other HEAD` for `git checkout
// other`. Measured on git 2.55.0.
const refSymrefPrefix = "ref:"

// refAdmitWorktreeBirth names the second head-symref carve-out on the pass
// line, beside `head=reattach` (loto-w0sx).
const refAdmitWorktreeBirth = "worktree-birth"

// The three names headWorktreeBirth reads off disk. `HEAD.lock` is git's own
// lock file for a HEAD about to be rewritten; `worktrees/` is the common
// dir's administrative directory, one subdirectory per linked worktree; and
// `locked` is the marker `git worktree add` writes there (with the text
// "initializing") for the span of the creation and then removes.
const (
	refHeadLockFile       = refHEAD + ".lock"
	refWorktreesDir       = "worktrees"
	refWorktreeLockedFile = "locked"
	refWorktreeGitdirFile = "gitdir"
)

// refOverrideWindow is how long after a refusal a checkout-wide claim by the
// same owner still counts as an override of it (§10b row 1). Ten minutes is
// the spec's number: long enough for a session to read the refusal, decide
// the guard was wrong and take the claim; short enough that a claim taken for
// an unrelated reason an hour later is not miscounted as an override.
const refOverrideWindow = 10 * time.Minute

// refCheckoutWidePrefix is the claim prefix that overrides I1 — the repo
// root, which CanonicalizePrefix spells ".". `loto claim . -t "..."` already
// documents itself as a takeover of the whole checkout.
const refCheckoutWidePrefix = "."

// refGuardOverrideEnv is the escape-hatch env var .githooks/hooks.d honors
// throughout (loto-mh07). For this guard it is read HERE, inside refVerdict,
// rather than by the shell dispatcher: git invokes the reference-transaction
// hook many times per operation — one per phase, plus a run of harmless
// zero-to-zero AUTO_MERGE transactions checkout emits (measured: 11 calls for
// one `git checkout` under two live sessions) — and refVerdict is reached
// only once, exactly when a refusal is about to fire. A shell-level check
// records once per HOOK INVOCATION; this records once per REFUSAL AVOIDED,
// which is the "guard bypassed" the events table is meant to count.
const refGuardOverrideEnv = "LOTO_GUARD_OVERRIDE"

// cmdHookRef is the `ref` arm of the hook router in cmd_hook.go. It takes
// git's phase as its one operand; the transaction itself arrives on stdin.
func cmdHookRef(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprint(stderr, hookUsageHead)
		return 2
	}
	if args[0] == subHelp || args[0] == "-h" || args[0] == flagHelpLong {
		fmt.Fprint(stdout, hookUsageHead)
		return 0
	}
	return runHookRef(ctx, args[0], os.Stdin, stdout, stderr)
}

// refUpdate is one line of git's reference-transaction stdin.
type refUpdate struct {
	Old string
	New string
	Ref string
}

// refRefusal binds a refused update to the shape that refused it.
//
// Collision carries the git dirs the birth carve-out could not tell apart
// when it refused a head-symref row — narrowed to the ones actually safe to
// name in a fix line: genuinely stale (mid-birth, unoccupied), never the
// common dir and never a born dir some checkout is sitting in, live or not
// (PR #354 review M1; PR #357 review B1/B3, which is why the narrowing
// happens before this field is ever set, not at print time). It may be
// empty even when the row was refused for exactly this reason — an ambiguous
// row with nothing SAFE to name prints no collision rows at all.
type refRefusal struct {
	Update    refUpdate
	Shape     string
	Collision []string
}

// refZeroOID reports whether an object id is git's all-zeros null oid. Length
// is not pinned: sha1 repos spell it 40 zeros, sha256 repos 64.
func refZeroOID(s string) bool {
	if s == "" {
		return false
	}
	return strings.Trim(s, "0") == ""
}

// classifyRefUpdate names the protected shape u matches, or "" if I1 admits
// it. The whole of `allow` from §5 lives here, and it is a three-shape
// DENYLIST, never deny-by-default: a shape nobody listed passes, so repo
// maintenance is not starved by a guard that has never heard of it.
//
// ‡ The branch-delete arm requires BOTH oids null, and that is a measurement,
// not a typo. On git 2.55.0 every route to deleting a branch — `git branch
// -D` on a loose ref, on a packed ref, and `git update-ref -d <ref> <oldoid>`
// with the old value stated — reports the update as `0000… 0000… refs/heads/x`.
// `git pack-refs --all` also emits `new=0` lines under refs/heads/, one per
// ref, as it removes each loose file AFTER the packed-refs file already holds
// it: those carry the real old oid, `<sha> 0000… refs/heads/x`. Refusing on
// `new=0` alone therefore breaks `git pack-refs` and `git gc` (measured: exit
// 128, "failed to run pack-refs") — which §5 forbids in the same sentence
// that defines the shape. The old-oid test is the only discriminator
// available to a hook that is given no operation name.
func classifyRefUpdate(u refUpdate) string {
	switch {
	case u.Ref == refHEAD && strings.HasPrefix(u.New, refSymrefPrefix):
		return refShapeHeadSymref
	case u.Ref == refStash:
		return refShapeStash
	case strings.HasPrefix(u.Ref, refHeadsPrefix) && refZeroOID(u.New) && refZeroOID(u.Old):
		return refShapeBranchDelete
	}
	return ""
}

// headReattach reports whether a head-symref update is a DETACHED HEAD being
// re-attached to the branch it is already sitting on — HEAD currently holds a
// bare oid, and the ref it is about to point at resolves to that same oid.
//
// ‡ This carve-out exists because refusing it strands the tree. `git rebase`
// ends by re-attaching HEAD after the branch tip has already moved (that tip
// move is an ordinary admit), so a refusal there leaves HEAD detached on a
// rebased branch — and the obvious recovery, `git checkout <branch>`, is a
// symref change that gets refused too. The operator is then stuck inside a
// guard meant to protect them.
//
// ‡ The discriminator is NOT the transaction's old oid. Measured on git
// 2.55.0, EVERY head-symref update reports `old` as all zeros — a plain
// `git checkout other`, `checkout -b`, `worktree add -b`, the detached
// re-attach and rebase's final re-attach alike — so keying on a zero old oid
// would admit every branch switch and I1 would guard nothing. The two facts
// that do separate them are read from the repo at hook time: HEAD is detached
// now, and the target resolves to what HEAD already holds. `checkout -b` fails
// the first test (HEAD is attached), which is why it stays refused even
// though it moves no file.
//
// Anything unreadable answers false — refuse — because this is the arm that
// WEAKENS the guard, and a guard must not weaken itself on a failed git call.
func headReattach(ctx context.Context, repoTop, newValue string) bool {
	target := strings.TrimPrefix(newValue, refSymrefPrefix)
	if target == "" {
		return false
	}
	// `symbolic-ref -q HEAD` exits non-zero exactly when HEAD is detached.
	if _, err := refGitOutput(ctx, repoTop, "symbolic-ref", "-q", refHEAD); err == nil {
		return false
	}
	head, err := refGitOutput(ctx, repoTop, "rev-parse", "--verify", "--quiet", refHEAD)
	if err != nil || head == "" {
		return false
	}
	want, err := refGitOutput(ctx, repoTop, "rev-parse", "--verify", "--quiet", target)
	if err != nil || want == "" {
		return false
	}
	return head == want
}

// headWorktreeBirth reports whether u — a head-symref update — is the HEAD of
// a worktree being CREATED rather than a HEAD some checkout is sitting in
// (loto-w0sx).
//
// ‡ Neither the row nor the environment can answer this on its own. Measured
// on git 2.55.0: `git worktree add <path> -b <branch>` from the shared
// checkout emits `0000… ref:refs/heads/<branch> HEAD` with GIT_DIR UNSET and
// cwd still the shared checkout — the same bytes and the same environment
// `git checkout -b <branch>` emits for the shared checkout's OWN head.
//
// The fact that does separate them is on disk: by the `prepared` phase git has
// already taken the lock on the exact HEAD it is about to write, AND WRITTEN
// THE NEW VALUE INTO IT. So the question "whose HEAD is this row?" has a
// direct answer — the git dir whose `HEAD.lock` holds this row's new value —
// and the carve-out admits only when that dir is a worktree still being built.
//
// ‡ Matching the row is what makes this safe, and the first cut of this
// carve-out did not (PR #354 review, F1). It asked only "does an unborn
// worktree dir exist?", so ANY head-symref row was admitted while one did —
// and `git branch -m <b> <b2>` on a branch a live peer has checked out
// rewrites THAT PEER's HEAD, with the caller's own HEAD never locked. Measured:
// the peer's git dir holds the matching `HEAD.lock`, the peer's dir is fully
// born, and the guard must refuse. Requiring the claimant to be UNIQUE is the
// other half: a stale unborn dir that happens to name the same branch cannot
// then vote a live worktree's HEAD through.
//
// ‡ Why uniqueness rather than "the first unborn dir wins": a worktree add
// killed mid-flight leaves HEAD.lock + `locked` + no HEAD behind FOREVER —
// `git worktree prune` refuses to reap a locked entry, by design (F2). The
// enabler is permanent, so the admission may not rest on its mere presence.
//
// ‡ There is deliberately no GIT_DIR-versus-common-dir test: `git worktree
// add` run from INSIDE a linked worktree reports that worktree's git dir and
// is still a birth (measured), and such a test would refuse it.
//
// Every failure direction answers false — refuse — because this is an arm that
// WEAKENS the guard. A ref backend that takes no HEAD.lock (reftable) finds no
// claimant and so keeps today's behavior rather than admitting everything.
// The bool return is the verdict; the []string is populated only when the
// verdict is false because two or more dirs claimed the same target (M1) —
// every other false direction (unreadable, not found, born, occupied) has
// nothing else to name and returns nil.
//
// ‡ The collision list is narrowed to SAFE claimants before it is ever
// returned (PR #357 review, B1/B3): the common dir claims its own row on
// every ordinary branch switch (git has already written its own new value
// into its own HEAD.lock by `prepared`), and a BORN dir — live or not — is
// somebody's real tree, not debris. Naming either in a remediation line reads
// as "delete your own .git internals" or "run `git worktree remove` on your
// peer's checkout": the exact destruction this guard exists to prevent.
func headWorktreeBirth(ctx context.Context, repoTop string, u refUpdate) (bool, []string) {
	want := refSymrefTarget(u.New)
	if want == "" {
		return false, nil
	}
	common, err := refGitOutput(ctx, repoTop, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common == "" {
		return false, nil
	}
	dirs, err := refGitDirs(common)
	if err != nil {
		return false, nil
	}
	claimants, err := refHeadLockClaimant(dirs, want)
	if err != nil || len(claimants) == 0 {
		return false, nil
	}
	if len(claimants) > 1 {
		return false, staleWorktreeClaimants(claimants, common)
	}
	claimant := claimants[0]
	return worktreeUnborn(claimant) && !worktreeOccupied(claimant), nil
}

// staleWorktreeClaimants narrows a HEAD.lock collision down to the dirs safe
// to name in a refusal: genuinely stale, mid-birth and unoccupied, never the
// common dir. It may return nil — a real ambiguity with nothing safe to
// name — and that is printed as no collision rows at all rather than a guess.
func staleWorktreeClaimants(claimants []string, common string) []string {
	var out []string
	for _, dir := range claimants {
		if dir == common {
			continue
		}
		if worktreeUnborn(dir) && !worktreeOccupied(dir) {
			out = append(out, dir)
		}
	}
	return out
}

// refSymrefTarget reads the ref name out of a symbolic-ref value. git spells
// it two ways and both arrive here: `ref:refs/heads/x` in a transaction row,
// `ref: refs/heads/x` inside a HEAD lock file.
func refSymrefTarget(v string) string {
	if !strings.HasPrefix(v, refSymrefPrefix) {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(v, refSymrefPrefix))
}

// refGitDirs lists every git dir in the checkout that owns a HEAD of its own:
// the common dir — the main worktree's — and one per linked worktree. A
// `worktrees/` directory that does not exist yet is not an error; a directory
// that cannot be READ is, because a claimant might be hiding in it.
func refGitDirs(common string) ([]string, error) {
	dirs := []string{common}
	entries, err := os.ReadDir(filepath.Join(common, refWorktreesDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return dirs, nil
		}
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(common, refWorktreesDir, e.Name()))
		}
	}
	return dirs, nil
}

// refHeadLockClaimant lists every git dir whose HEAD.lock is about to write
// want. Zero means the HEAD in flight cannot be located; more than one means
// two HEAD writes to the same branch are racing, and the caller names both so
// the operator can tell which dir is the stale one. A HEAD.lock that cannot
// be read for a reason other than absence aborts the whole scan — an error
// here has nothing to say about who else claims want, so it is not folded
// into a "zero claimants" verdict.
func refHeadLockClaimant(dirs []string, want string) ([]string, error) {
	var found []string
	for _, dir := range dirs {
		raw, err := os.ReadFile(filepath.Join(dir, refHeadLockFile))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if refSymrefTarget(strings.TrimSpace(string(raw))) != want {
			continue
		}
		found = append(found, dir)
	}
	return found, nil
}

// worktreeUnborn reports whether an administrative worktree directory is one
// `git worktree add` is still building: the HEAD file itself has not been
// written, and git's creation marker is present.
//
// Both are required, and the caller has already established that the dir
// holds the HEAD lock. A missing HEAD alone would also describe a worktree
// whose HEAD file was lost, which is somebody's real tree; the marker alone
// describes `git worktree lock`, which any finished worktree may carry. The
// marker's TEXT is not matched: "initializing" is git's wording today, and
// pinning it would turn a future rewording into a silent return of this bug
// rather than into the other conditions doing their job.
//
// The common dir is never unborn — it has no `locked` file and always has a
// HEAD — so the main worktree's own branch switch falls out here.
func worktreeUnborn(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, refHEAD)); !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, refWorktreeLockedFile))
	return err == nil
}

// worktreeOccupied reports whether a live session sits in the worktree the
// administrative directory names — I1's question, asked of that checkout
// rather than of this one. A path that cannot be read counts as occupied.
func worktreeOccupied(dir string) bool {
	path := refWorktreePath(dir)
	if path == "" {
		return true
	}
	return len(identity.LiveOwnerUUIDsInRepo(path)) > 0
}

// refWorktreePath reads the working-tree path an administrative dir's
// `gitdir` file names — the path `git worktree remove` takes — or "" when it
// cannot be read. Shared by worktreeOccupied and the M1 refusal message.
func refWorktreePath(dir string) string {
	raw, err := os.ReadFile(filepath.Join(dir, refWorktreeGitdirFile))
	if err != nil {
		return ""
	}
	inner := strings.TrimSpace(string(raw))
	if inner == "" {
		return ""
	}
	return filepath.Dir(inner)
}

// refGitOutput runs one short git query for the hook, in repoTop.
func refGitOutput(ctx context.Context, repoTop string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	if repoTop != "" {
		cmd.Dir = repoTop
	}
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// parseRefTransaction reads git's "<old> <new> <ref>" lines. A ref name may
// not contain a space (git's own check-ref-format rule), so cutting on the
// first two spaces is exact rather than approximate — and it keeps a symref
// value like "ref:refs/heads/other" intact in the middle field.
//
// A malformed line is skipped rather than fatal: this reader's job is to find
// protected shapes, and a line it cannot parse is a line it cannot claim to
// have judged. Every failure direction here is "pass", which is the fail-open
// stance the rest of loto's gates take.
func parseRefTransaction(r io.Reader) []refUpdate {
	var out []refUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		oldOID, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		newOID, ref, ok := strings.Cut(rest, " ")
		if !ok || ref == "" {
			continue
		}
		out = append(out, refUpdate{Old: oldOID, New: newOID, Ref: ref})
	}
	return out
}

// runHookRef is the IO runner. Order of the checks is a cost decision as much
// as a logic one: the shape test is pure string work over a handful of lines,
// so the overwhelmingly common transaction — a commit moving a branch tip —
// exits without reading the session directory or opening the store at all.
func runHookRef(ctx context.Context, phase string, stdin io.Reader, stdout, stderr io.Writer) int {
	updates := parseRefTransaction(stdin)

	// Only `prepared` can refuse: git aborts the transaction there and nothing
	// has been written. `committed` and `aborted` are after the fact, and
	// `preparing` has not resolved symrefs yet.
	if phase != "prepared" {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d phase=%s\n", len(updates), phase)
		return 0
	}

	shaped := refShapedUpdates(updates)
	if len(shaped) == 0 {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d\n", len(updates))
		return 0
	}

	// A hook with no session id is not in S and claims no protection: a bare
	// shell, a cron job, a human's terminal. It passes (§5 I1).
	if identity.SessionIDFromEnv() == "" {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d session=absent\n", len(updates))
		return 0
	}

	// The checkout. It scopes S and answers the re-attach question; without it
	// neither can be asked, so an unresolvable toplevel passes.
	repoTop, err := repoTopForCwd(ctx)
	if err != nil || repoTop == "" {
		fmt.Fprintf(stderr, "⚠ repo=unreadable ref-guard=fail-open\n")
		return 3
	}

	refusals := refDropReattach(ctx, repoTop, shaped)
	if len(refusals) == 0 {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d head=reattach\n", len(updates))
		return 0
	}

	// S — live sessions in THIS checkout, from session records rather than
	// call records, so two idle peers still protect each other (§3, spec test
	// 13) and a session live in an unrelated repo protects nothing here.
	live := identity.LiveOwnerUUIDsInRepo(repoTop)
	if len(live) < 2 {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d\n", len(updates), len(live))
		return 0
	}

	// The second head-symref carve-out: the HEAD of a worktree being created.
	// It sits AFTER the live gate on purpose — it reads directories and a lock
	// file, and a single-session checkout, which is most of them, must pay
	// nothing for a refusal that was never going to fire.
	// ref_admitted is a per-TRANSACTION signal (§10b row 1 reads it as a ratio
	// against ref_refused), so it is written only once the rest of the
	// transaction is known to pass too — a birth riding beside a row that
	// stays refused writes no event, because the carve-out let nothing
	// through in the end (PR #354 review, L1).
	kept, admitted := refDropWorktreeBirth(ctx, repoTop, refusals)
	if len(kept) == 0 {
		if len(admitted) > 0 {
			recordRefBirthAdmits(ctx, admitted, len(live), stderr)
		}
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d head=%s\n", len(updates), len(live), refAdmitWorktreeBirth)
		return 0
	}
	return refVerdict(ctx, kept, live, len(updates), stdout, stderr)
}

// refShapedUpdates keeps the updates matching a protected shape.
func refShapedUpdates(updates []refUpdate) []refRefusal {
	out := make([]refRefusal, 0, len(updates))
	for _, u := range updates {
		if shape := classifyRefUpdate(u); shape != "" {
			out = append(out, refRefusal{Update: u, Shape: shape})
		}
	}
	return out
}

// refDropReattach removes the one head-symref case I1 admits: a detached HEAD
// being re-attached where it already sits (see headReattach).
func refDropReattach(ctx context.Context, repoTop string, shaped []refRefusal) []refRefusal {
	out := make([]refRefusal, 0, len(shaped))
	for _, r := range shaped {
		if r.Shape == refShapeHeadSymref && headReattach(ctx, repoTop, r.Update.New) {
			continue
		}
		out = append(out, r)
	}
	return out
}

// refDropWorktreeBirth splits refusals into the ones that stand and the
// head-symref rows admitted as a worktree being born. Every OTHER row in the
// same transaction — a stash, a branch delete, a second head-symref naming a
// different branch — stays in `kept`: the carve-out is per row, and admitting
// one row never admits its neighbours.
func refDropWorktreeBirth(ctx context.Context, repoTop string, refusals []refRefusal) (kept, admitted []refRefusal) {
	for i := range refusals {
		r := refusals[i]
		if r.Shape == refShapeHeadSymref {
			admit, collision := headWorktreeBirth(ctx, repoTop, r.Update)
			if admit {
				admitted = append(admitted, r)
				continue
			}
			r.Collision = collision
		}
		kept = append(kept, r)
	}
	return kept, admitted
}

// recordRefBirthAdmits writes one ref_admitted event per row the birth
// carve-out let through. I1's counters are a RATIO, and a weakening nobody
// counts cannot be read back: the staged-lock promotion read needs to see how
// often this arm fires, and against what, to tell a carve-out doing its job
// from one being leaned on (PR #354 review, F5).
//
// It opens its own runtime because the admit path otherwise never needs the
// store. Failing to record is reported and never changes the verdict.
func recordRefBirthAdmits(ctx context.Context, admitted []refRefusal, live int, stderr io.Writer) {
	rt, err := openRuntime(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ ref-admitted-counter=unrecorded err=%q\n", err)
		return
	}
	defer rt.Close()
	now := time.Now()
	for i := range admitted {
		r := admitted[i]
		detail, err := json.Marshal(refRefusedDetail{
			Old: r.Update.Old, New: r.Update.New, Ref: r.Update.Ref, Live: live,
			Repo: rt.RepoTop,
		})
		if err != nil {
			detail = nil
		}
		if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
			Kind:      store.EventRefAdmitted,
			Target:    domain.Target{Canonical: r.Update.Ref},
			ActorUUID: rt.Agent.UUID,
			Reason:    refAdmitWorktreeBirth,
			Detail:    string(detail),
			CreatedAt: now,
		}); err != nil {
			fmt.Fprintf(stderr, "⚠ ref-admitted-counter=unrecorded err=%q\n", err)
		}
	}
}

// refVerdict is the half that needs the store: the override claim, the
// counter, and the refusal itself.
func refVerdict(ctx context.Context, refusals []refRefusal, live map[string]struct{}, updates int, stdout, stderr io.Writer) int {
	rt, err := openRuntime(ctx)
	if err != nil {
		// Fail-open, loudly. Without the store the override claim cannot be
		// read, and refusing every branch switch because loto's own database
		// is unreachable is the gate becoming the outage.
		fmt.Fprintf(stderr, "⚠ store=unreachable ref-guard=fail-open err=%q\n", err)
		return 3
	}
	defer rt.Close()

	if held, holder := refCheckoutWideClaim(rt, stderr); held {
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d override=claim holder=%s\n",
			updates, len(live), holder)
		return 0
	}

	// The env-var override (loto-mh07): honored, but the claim above is still
	// preferred (checked first) because it names its reason in the refusal it
	// overrides and this does not. Fires once — refVerdict is reached only at
	// the one invocation that was actually about to refuse.
	if os.Getenv(refGuardOverrideEnv) == "1" {
		if err := rt.Store.RecordGuardOverride(rt.Ctx, rt.Agent.UUID, hookOverrideGuardReferenceTransaction); err != nil {
			fmt.Fprintf(stderr, "⚠ guard-override=unrecorded guard=%s err=%q\n", hookOverrideGuardReferenceTransaction, err)
		}
		fmt.Fprintf(stdout, "✓ ref-allowed count=%d live=%d override=env\n", updates, len(live))
		return 0
	}

	recordRefRefusals(rt, refusals, len(live), stderr)
	printRefRefusal(stderr, refusals, rt.Agent.UUID, live)
	return 1
}

// refCheckoutWideClaim reports whether this caller holds the live
// checkout-wide claim. Kin counts: a subagent stamped by the dispatch hook
// resolves to its parent's owner (§3), so a lane's fan-out is covered by the
// claim the lane took.
//
// ‡ Both reads fail OPEN — an unreadable claims table or an unresolvable
// parent answers "the override is held", not "refuse". They are the same
// question the store-unreachable path already answers that way, asked one
// layer in: loto not being able to read its own state must not turn into a
// refusal the operator cannot even override, since taking the override needs
// the very table that just failed to read.
func refCheckoutWideClaim(rt *runtime, stderr io.Writer) (bool, string) {
	claims, err := rt.Store.ListClaims(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stderr, "⚠ claims=unreadable ref-guard=fail-open err=%q\n", err)
		return true, "unknown"
	}
	mine := map[string]struct{}{rt.Agent.UUID: {}}
	kin, kerr := parentKin(rt.Ctx)
	if kerr != nil {
		fmt.Fprintf(stderr, "⚠ kin=unresolved ref-guard=fail-open err=%q\n", kerr)
		return true, "unknown"
	}
	for _, k := range kin {
		mine[string(k)] = struct{}{}
	}
	now := time.Now()
	for i := range claims {
		c := claims[i]
		if c.PathPrefix != refCheckoutWidePrefix || c.Expired(now) {
			continue
		}
		if _, ok := mine[string(c.OwnerUUID)]; ok {
			return true, string(c.OwnerUUID)
		}
	}
	return false, ""
}

// refRefusedDetail is the §10b row-1 payload: the transaction git offered and
// how many sessions were live when it was refused.
// Repo is carried because loto's store is per PROJECT, not per checkout: two
// worktrees of one repo share it, so the refusal has to say which checkout it
// happened in for the override counter to pair the two honestly.
type refRefusedDetail struct {
	Old  string `json:"old"`
	New  string `json:"new"`
	Ref  string `json:"ref"`
	Live int    `json:"live"`
	Repo string `json:"repo,omitempty"`
}

// recordRefRefusals writes one ref_refused event per refused update. Failing
// to record is reported and never changes the verdict: a lost counter row
// costs the telemetry a data point, and letting it undo the refusal would
// cost a peer their working tree.
func recordRefRefusals(rt *runtime, refusals []refRefusal, live int, stderr io.Writer) {
	now := time.Now()
	for i := range refusals {
		r := refusals[i]
		detail, err := json.Marshal(refRefusedDetail{
			Old: r.Update.Old, New: r.Update.New, Ref: r.Update.Ref, Live: live,
			Repo: rt.RepoTop,
		})
		if err != nil {
			detail = nil
		}
		// ‡ AppendEventRotating, not AppendEvent: this fires on ref
		// transactions in a busy shared checkout and nothing else on the path
		// trims the table.
		if _, err := rt.Store.AppendEventRotating(rt.Ctx, domain.Event{
			Kind:      store.EventRefRefused,
			Target:    domain.Target{Canonical: r.Update.Ref},
			ActorUUID: rt.Agent.UUID,
			Reason:    r.Shape,
			Detail:    string(detail),
			CreatedAt: now,
		}); err != nil {
			fmt.Fprintf(stderr, "⚠ ref-refused-counter=unrecorded err=%q\n", err)
		}
	}
}

// printRefRefusal renders the refusal. It goes to stderr because that is what
// git relays to whoever ran the command, and it names three things the reader
// needs: which shape was refused, who else is live, and the exact override.
//
// ‡ ONE fix block (PR #357 review, M2): a collision's remediation and the
// checkout-wide-claim override are both things to run, and two separate
// ```bash fences read as two unrelated fixes rather than "try this, or this."
func printRefRefusal(stderr io.Writer, refusals []refRefusal, self string, live map[string]struct{}) {
	fmt.Fprintf(stderr, "✗ ref-refused count=%d live=%d\n", len(refusals), len(live))
	rows := make([]string, 0, len(refusals))
	for i := range refusals {
		r := refusals[i]
		rows = append(rows, fmt.Sprintf("✗ ref=%s shape=%s old=%s new=%s",
			r.Update.Ref, r.Shape, r.Update.Old, r.Update.New))
	}
	sort.Strings(rows)
	for _, row := range rows {
		fmt.Fprintln(stderr, row)
	}
	peers := make([]string, 0, len(live))
	for uuid := range live {
		if uuid != self {
			peers = append(peers, uuid)
		}
	}
	sort.Strings(peers)
	fmt.Fprintf(stderr, "ℹ live-peers=%s\n", strings.Join(peers, ","))
	fmt.Fprintln(stderr, "ℹ this move would rewrite their working tree; a stash or branch delete is not undoable for them")
	collisionFix := printRefCollisionRows(stderr, refusals)
	fmt.Fprintln(stderr, "ℹ override=checkout-wide-claim")
	fmt.Fprintln(stderr, "```bash")
	for _, line := range collisionFix {
		fmt.Fprintln(stderr, line)
	}
	if len(collisionFix) > 0 {
		fmt.Fprintln(stderr, `# or: loto claim . -t "<reason>"`)
	} else {
		fmt.Fprintln(stderr, `loto claim . -t "<reason>"`)
	}
	fmt.Fprintln(stderr, "```")
}

// printRefCollisionRows renders the M1 case's ℹ rows — a head-symref row
// refused because two or more HEAD.lock claimants named its target, so the
// refusal is not an ordinary tree move to undo but a stale worktrees/<n> dir
// sitting on a branch name (most often a `worktree add` killed mid-flight,
// which `git worktree prune` will not reap because its `locked` marker is on
// purpose) — and returns the commands to clear each one, to be folded into
// the caller's single fix block. Silent, and returns nil, when no refusal
// carries a collision at all, which is most of them.
//
// ‡ `rm -rf .git/worktrees/<n>` is the primary fix: the staleWorktreeClaimants
// filter (line 301) guarantees the dir is unborn and unoccupied, so it is an
// admin directory that git worktree remove cannot validate on a mid-birth
// crash. `git worktree unlock && remove` (B2) is offered as a fallback for a
// working tree that exists; unlocking first handles both a locked live tree
// and a crashed one.
func printRefCollisionRows(stderr io.Writer, refusals []refRefusal) []string {
	type staleDir struct {
		ref, name, target string
	}
	var rows []staleDir
	seen := map[string]bool{}
	for i := range refusals {
		r := refusals[i]
		for _, dir := range r.Collision {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			target := refWorktreePath(dir)
			if target == "" {
				continue
			}
			rows = append(rows, staleDir{
				ref:    r.Update.Ref,
				name:   filepath.Join(refWorktreesDir, filepath.Base(dir)),
				target: target,
			})
		}
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].ref != rows[j].ref {
			return rows[i].ref < rows[j].ref
		}
		return rows[i].name < rows[j].name
	})
	fix := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		fmt.Fprintf(stderr, "ℹ ref=%s stale-dir=%s\n", row.ref, row.name)
		q := shellQuote(row.target)
		fix = append(fix,
			"rm -rf "+shellQuote(filepath.Join(".git", row.name)),
			"# or: git worktree unlock "+q+" && git worktree remove "+q,
		)
	}
	return fix
}
