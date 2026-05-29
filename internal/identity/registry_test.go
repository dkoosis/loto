package identity

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestEnsureBlankAgentIDIsEphemeral(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID == "" || a.Handle == "" {
		t.Fatalf("agent missing fields: %+v", a)
	}
	path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("ephemeral agent must not persist; got err=%v", err)
	}
}

func TestEnsurePersistsWhenAgentIDUnset(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file missing: %v", err)
	}
}

// Regression for gh#121: with both LOTO_AGENT_ID and CLAUDE_CODE_SESSION_ID
// unset, Ensure must mint a fresh agent even if a recent (fresh) record
// exists on disk. The prior heuristic adopted within-24h records, which
// could resurrect a dead session's UUID and re-attribute new locks to it.
func TestEnsureNoFallbackToRecentAgent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	host, _ := os.Hostname()
	prior := &Agent{
		UUID:      newUUID(),
		Handle:    "PriorPanda",
		Host:      host,
		CreatedAt: time.Now().Add(-time.Hour).UTC(), // fresh, within old 24h window
	}
	if err := writeAgent(prior); err != nil {
		t.Fatal(err)
	}

	got, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.UUID == prior.UUID {
		t.Fatalf("Ensure adopted recent on-disk agent %s — heuristic fallback must be gone (gh#121)", prior.UUID)
	}
}

// TestEnsureStaleAgentIDIsHardError asserts that an explicit LOTO_AGENT_ID
// pointing at a uuid with no registry record fails loudly rather than
// silently substituting an ephemeral identity. Silent substitution orphans
// every lock acquired in the session because the next invocation sees a
// different uuid (audit loto-16t / governing principle: ambiguity never
// authority).
func TestEnsureStaleAgentIDIsHardError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")
	t.Setenv("LOTO_AGENT_ID", "11111111-2222-4333-8444-555555555555")

	_, err := Ensure(context.Background())
	if !errors.Is(err, errStaleAgentID) {
		t.Fatalf("want errStaleAgentID, got %v", err)
	}
}

// TestEnsureRejectsMalformedAgentID asserts that a syntactically invalid
// LOTO_AGENT_ID is rejected before any filesystem interaction. Without
// this, `LOTO_AGENT_ID=../../etc/passwd` would escape registryDir() via
// filepath.Join in loadByUUID.
func TestEnsureRejectsMalformedAgentID(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	for _, bad := range []string{"not-a-uuid", "../../escape", "agent-123", "11111111"} {
		t.Setenv("LOTO_AGENT_ID", bad)
		_, err := Ensure(context.Background())
		if !errors.Is(err, errInvalidAgentID) {
			t.Errorf("LOTO_AGENT_ID=%q: want errInvalidAgentID, got %v", bad, err)
		}
	}
}

// TestMostRecentAgentSkipsStale asserts the freshness gate: an agent record
// older than fallbackFreshness is not reused as the unset+unset fallback.
// Without this gate, a CLI invocation a week after the last session would
// silently re-attribute new locks to a long-dead identity.
func TestMostRecentAgentSkipsStale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	host, _ := os.Hostname()
	old := &Agent{UUID: newUUID(), Handle: "OldOwl", Host: host, CreatedAt: time.Now().Add(-72 * time.Hour).UTC()}
	if err := writeAgent(old); err != nil {
		t.Fatal(err)
	}

	got, err := mostRecentAgent(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("stale record returned as fallback: %+v", got)
	}
}

// TestGCPreservesSessionReferencedAgents asserts the binding invariant:
// even if an agent file's mtime predates the GC cutoff, if a session cache
// still references it, GC must not delete it. Breaking this binding would
// leave a live session pointing at a missing uuid → next Ensure(context.Background()) in that
// session would error out of nowhere.
func TestGCPreservesSessionReferencedAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "preserve-me")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the agent file past the GC cutoff.
	agentPath := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(agentPath, old, old); err != nil {
		t.Fatal(err)
	}

	if err := gcStaleAgents(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Fatalf("session-referenced agent was deleted: %v", err)
	}
}

// TestGCPreservesLockReferencedAgents asserts that even when an agent file's
// mtime predates the GC cutoff, if any live lock row still references its
// uuid as owner_uuid, GC must not delete it. Pruning a lock-pinned agent
// strands the lock with an unresolvable owner: LookupByUUID(holder) returns
// ENOENT for the live holder, breaking render of conflict reports and any
// holder-scoped operation. Regression for gh#125 (loto-ffg).
func TestGCPreservesLockReferencedAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	// A stale-by-time agent file that is nonetheless pinned by a live lock.
	stale := &Agent{
		UUID:      newUUID(),
		Handle:    "PinnedPanda",
		CreatedAt: time.Now().Add(-90 * 24 * time.Hour).UTC(),
		Host:      "old",
	}
	if err := writeAgent(stale); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(dir, ".loto", "agents", stale.UUID+".json")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}

	pinned := map[string]struct{}{stale.UUID: {}}
	if err := gcStaleAgents(time.Now(), pinned); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); err != nil {
		t.Fatalf("lock-pinned agent was deleted: %v", err)
	}
}

func TestEnsureRespectsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	first, _ := Ensure(context.Background())
	t.Setenv("LOTO_AGENT_ID", first.UUID)
	second, _ := Ensure(context.Background())
	if second.UUID != first.UUID {
		t.Fatalf("Ensure(context.Background()) must return same uuid when LOTO_AGENT_ID is set; %s != %s", second.UUID, first.UUID)
	}
}

func TestEnsureHonorsLOTOHandle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("LOTO_HANDLE", "TeamTrixiAbc")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Handle != "TeamTrixiAbc" {
		t.Fatalf("handle: got %q want TeamTrixiAbc", a.Handle)
	}
}

func TestEnsureRejectsInvalidLOTOHandle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")
	for _, bad := range []string{"lowercase", "no spaces here", "Bad_Underscore", "Single"} {
		t.Setenv("LOTO_HANDLE", bad)
		if _, err := Ensure(context.Background()); err == nil {
			t.Errorf("Ensure must reject LOTO_HANDLE=%q", bad)
		}
	}
}

func TestGCStaleAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	fresh, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	freshPath := filepath.Join(dir, ".loto", "agents", fresh.UUID+".json")

	// Manually drop a stale record into the registry.
	stale := &Agent{UUID: newUUID(), Handle: "StaleAgent", CreatedAt: time.Now().Add(-90 * 24 * time.Hour).UTC(), Host: "old"}
	if err := writeAgent(stale); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(dir, ".loto", "agents", stale.UUID+".json")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(stalePath, old, old); err != nil {
		t.Fatal(err)
	}

	if err := gcStaleAgents(time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Fatalf("stale agent not removed: err=%v", err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh agent must survive GC: %v", err)
	}
}

// TestEnsureDistinctClaudeSessions asserts that two Claude Code sessions on
// the same host resolve to distinct identities. Each Claude session exports a
// unique CLAUDE_CODE_SESSION_ID; Ensure(context.Background()) consumes that signal so concurrent
// sessions do not collapse onto a shared owner_uuid via mostRecentAgent.
func TestEnsureDistinctClaudeSessions(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("CLAUDECODE", "1")
	// LOTO_AGENT_ID intentionally unset — simulates Claude Bash tool calls
	// where no per-session env-var bridge is configured.
	if _, ok := os.LookupEnv("LOTO_AGENT_ID"); ok {
		t.Setenv("LOTO_AGENT_ID", "")
		os.Unsetenv("LOTO_AGENT_ID")
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-aaaa-1111")
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-bbbb-2222")
	b, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	if a.UUID == b.UUID {
		t.Fatalf("two distinct CLAUDE_CODE_SESSION_ID values produced the same uuid %q — sessions collide via mostRecentAgent fallback (gh#45)", a.UUID)
	}

	// Same session id repeated → same uuid (stable per session).
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-aaaa-1111")
	a2, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a2.UUID != a.UUID {
		t.Fatalf("same CLAUDE_CODE_SESSION_ID must produce stable uuid; got %s want %s", a2.UUID, a.UUID)
	}
}

// TestWriteAgentAtomic asserts concurrent readers never see partial/empty
// JSON while writeAgent rewrites the same record. Pre-fix (os.WriteFile
// truncate-then-write), readers racing the writer observe zero-byte reads
// or short writes and fail Unmarshal → mostRecentAgent silently drops the
// entry → identity flap (gh#50 / loto-200).
// TestSyncDir asserts the parent-dir fsync helper succeeds on a real
// directory and surfaces an error for a path that cannot be opened
// (loto-cq6 / gh#131). Durability across power-loss is not observable from
// userspace without fault injection, so this only covers the helper's own
// open→sync→close contract; regression coverage for the publish sites comes
// from TestWriteAgentAtomic and TestEnsureSessionCachePersists.
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

	// Missing dir: created and usable.
	fresh := filepath.Join(base, "fresh")
	if err := mkdirAllSync(fresh); err != nil {
		t.Fatalf("mkdirAllSync on missing dir: %v", err)
	}
	if fi, err := os.Stat(fresh); err != nil || !fi.IsDir() {
		t.Fatalf("mkdirAllSync did not create dir: stat=%v err=%v", fi, err)
	}

	// Idempotent on an existing dir.
	if err := mkdirAllSync(fresh); err != nil {
		t.Fatalf("mkdirAllSync on existing dir: %v", err)
	}

	// Path exists as a file → MkdirAll's "not a directory" error must surface.
	asFile := filepath.Join(base, "afile")
	if err := os.WriteFile(asFile, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := mkdirAllSync(asFile); err == nil {
		t.Fatal("mkdirAllSync over a file: want error, got nil")
	}
}

func TestWriteAgentAtomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	done := make(chan struct{}, 2)

	// Writer: rewrite the same record repeatedly.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			select {
			case <-stop:
				return
			default:
				if err := writeAgent(a); err != nil {
					t.Errorf("writeAgent: %v", err)
					return
				}
			}
		}
	}()

	// Reader: read+unmarshal repeatedly. Any partial read is a failure.
	var partial int
	go func() {
		defer func() { done <- struct{}{} }()
		path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
		for range 2000 {
			select {
			case <-stop:
				return
			default:
			}
			body, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			var got Agent
			if err := json.Unmarshal(body, &got); err != nil {
				partial++
			}
		}
	}()

	<-done
	close(stop)
	<-done

	if partial > 0 {
		t.Fatalf("writeAgent not atomic: %d partial reads observed", partial)
	}
}

// TestEnsureSessionCachePersists asserts a session cache file is created at
// ~/.loto/session/$SID.json and used on subsequent calls — concurrent calls
// within one Claude session must NOT mint new identities each time
// (loto-aa6 / gh#41).
func TestEnsureSessionCachePersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-cache-test")

	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".loto", "session", "session-cache-test.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("session cache not written: %v", err)
	}
	b, err := Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != b.UUID {
		t.Fatalf("session cache not honored: %s != %s", a.UUID, b.UUID)
	}
}

// TestEnsureForSessionFirstUseRace asserts that N concurrent Ensure(context.Background()) calls
// for the same CLAUDE_CODE_SESSION_ID converge on one uuid — without this
// guarantee, two processes both miss the cache, both mint, both write, and
// one becomes a brief orphan (gh#28).
func TestEnsureForSessionFirstUseRace(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "race-sid")

	const N = 20
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		uuids = make(map[string]int, N)
		errs  []error
		start = make(chan struct{})
	)
	for range N {
		wg.Go(func() {
			<-start
			a, err := Ensure(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			uuids[a.UUID]++
		})
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(uuids) != 1 {
		t.Fatalf("concurrent ensureForSession produced %d distinct uuids; want 1: %v", len(uuids), uuids)
	}

	agentsDir := filepath.Join(dir, ".loto", "agents")
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		t.Fatalf("agents dir: %v", err)
	}
	jsonCount := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			jsonCount++
		}
	}
	if jsonCount != 1 {
		t.Fatalf("orphan agent files: got %d, want 1", jsonCount)
	}
}

// TestEnsureForSessionRespectsCtxCancel asserts that a cancelled context
// aborts the ensureForSession retry loop within one poll interval (~5ms)
// rather than spinning through all 20 retries (~100ms). Without this fix,
// Ctrl-C / parent deadline cannot abort the retry (gh#114).
func TestEnsureForSessionRespectsCtxCancel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "ctx-cancel-test")

	// Pre-create session dir and claim the sid with O_EXCL so ensureForSession
	// enters the retry branch. Leave the file empty (0 bytes) so loadSessionAgent
	// always fails — simulates a winner that crashed mid-write.
	if err := os.MkdirAll(filepath.Join(dir, ".loto", "session"), 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".loto", "session", "ctx-cancel-test.json")
	f, err := os.OpenFile(cachePath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Close() // 0-byte file — loadSessionAgent will fail every retry

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	start := time.Now()
	_, err = Ensure(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Ensure with cancelled ctx must return error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	// With 20 retries × 5ms sleep, the old code takes ~100ms. A ctx-aware
	// loop should bail in <10ms.
	if elapsed > 50*time.Millisecond {
		t.Fatalf("retry loop took %v — ctx.Done() not checked in retry", elapsed)
	}
}

// TestEnsureForSessionRecoverZeroByteCacheOnWinnerCrash asserts that when
// the winner crashes between O_EXCL create and Write (leaving a 0-byte
// session cache), the loser recovers by unlinking the corrupt cache and
// re-claiming the session — rather than surfacing errNoSessionCache forever
// (gh#115 / loto-yeni).
func TestEnsureForSessionRecoverZeroByteCacheOnWinnerCrash(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")

	sid := "winner-crashed"

	sessDir := filepath.Join(dir, ".loto", "session")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(sessDir, sid+".json")
	f, err := os.Create(cachePath)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure must recover from 0-byte cache, got: %v", err)
	}
	if a.UUID == "" {
		t.Fatal("recovered agent has empty UUID")
	}

	b, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("second Ensure after recovery failed: %v", err)
	}
	if b.UUID != a.UUID {
		t.Fatalf("identity unstable after recovery: %s != %s", b.UUID, a.UUID)
	}
}

// TestEnsureForSessionRecoverUnparseableCacheOnWinnerCrash covers the
// partial-write variant: winner wrote some bytes but crashed before
// completing valid JSON.
func TestEnsureForSessionRecoverUnparseableCacheOnWinnerCrash(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")

	sid := "winner-partial-write"

	sessDir := filepath.Join(dir, ".loto", "session")
	if err := os.MkdirAll(sessDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(sessDir, sid+".json")
	if err := os.WriteFile(cachePath, []byte(`{"uuid":"trunc`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", sid)
	a, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("Ensure must recover from unparseable cache, got: %v", err)
	}
	if a.UUID == "" {
		t.Fatal("recovered agent has empty UUID")
	}

	b, err := Ensure(context.Background())
	if err != nil {
		t.Fatalf("second Ensure after recovery failed: %v", err)
	}
	if b.UUID != a.UUID {
		t.Fatalf("identity unstable after recovery: %s != %s", b.UUID, a.UUID)
	}
}

// TestEnsureHomeUnsetYieldsAbsolutePath asserts that when HOME is unset,
// registryDir/sessionDir still return absolute paths rooted in
// os.UserHomeDir() — not relative ".loto/agents" fragments that change
// meaning with cwd (gh#112 / loto-3axo).
func TestEnsureHomeUnsetYieldsAbsolutePath(t *testing.T) {
	t.Setenv("HOME", "")
	os.Unsetenv("HOME")
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	rdir := registryDir()
	if !filepath.IsAbs(rdir) {
		t.Fatalf("registryDir() returned relative path %q when HOME unset", rdir)
	}
	sdir := sessionDir()
	if !filepath.IsAbs(sdir) {
		t.Fatalf("sessionDir() returned relative path %q when HOME unset", sdir)
	}
}

// TestRegistryDirIsAlwaysAbsolute asserts the guard: even if os.UserHomeDir()
// somehow returned "", registryDir must not silently yield a relative path.
func TestRegistryDirIsAlwaysAbsolute(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)

	rdir := registryDir()
	if !filepath.IsAbs(rdir) {
		t.Fatalf("registryDir() not absolute: %q", rdir)
	}
	sdir := sessionDir()
	if !filepath.IsAbs(sdir) {
		t.Fatalf("sessionDir() not absolute: %q", sdir)
	}
}
