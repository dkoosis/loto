package identity

import (
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

	a, err := Ensure()
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

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("identity file missing: %v", err)
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

	_, err := Ensure()
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
		_, err := Ensure()
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
// leave a live session pointing at a missing uuid → next Ensure() in that
// session would error out of nowhere.
func TestGCPreservesSessionReferencedAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	t.Setenv("CLAUDE_CODE_SESSION_ID", "preserve-me")

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the agent file past the GC cutoff.
	agentPath := filepath.Join(dir, ".loto", "agents", a.UUID+".json")
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(agentPath, old, old); err != nil {
		t.Fatal(err)
	}

	if err := gcStaleAgents(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(agentPath); err != nil {
		t.Fatalf("session-referenced agent was deleted: %v", err)
	}
}

func TestEnsureRespectsExistingEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	first, _ := Ensure()
	t.Setenv("LOTO_AGENT_ID", first.UUID)
	second, _ := Ensure()
	if second.UUID != first.UUID {
		t.Fatalf("Ensure() must return same uuid when LOTO_AGENT_ID is set; %s != %s", second.UUID, first.UUID)
	}
}

func TestEnsureHonorsLOTOHandle(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")
	t.Setenv("LOTO_HANDLE", "TeamTrixiAbc")

	a, err := Ensure()
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
		if _, err := Ensure(); err == nil {
			t.Errorf("Ensure must reject LOTO_HANDLE=%q", bad)
		}
	}
}

func TestGCStaleAgents(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	os.Unsetenv("LOTO_AGENT_ID")
	os.Unsetenv("CLAUDE_CODE_SESSION_ID")

	fresh, err := Ensure()
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

	if err := gcStaleAgents(time.Now()); err != nil {
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
// unique CLAUDE_CODE_SESSION_ID; Ensure() consumes that signal so concurrent
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
	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-bbbb-2222")
	b, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}

	if a.UUID == b.UUID {
		t.Fatalf("two distinct CLAUDE_CODE_SESSION_ID values produced the same uuid %q — sessions collide via mostRecentAgent fallback (gh#45)", a.UUID)
	}

	// Same session id repeated → same uuid (stable per session).
	t.Setenv("CLAUDE_CODE_SESSION_ID", "session-aaaa-1111")
	a2, err := Ensure()
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
func TestWriteAgentAtomic(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("LOTO_AGENT_ID", "")

	a, err := Ensure()
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

	a, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(dir, ".loto", "session", "session-cache-test.json")
	if _, err := os.Stat(cachePath); err != nil {
		t.Fatalf("session cache not written: %v", err)
	}
	b, err := Ensure()
	if err != nil {
		t.Fatal(err)
	}
	if a.UUID != b.UUID {
		t.Fatalf("session cache not honored: %s != %s", a.UUID, b.UUID)
	}
}

// TestEnsureForSessionFirstUseRace asserts that N concurrent Ensure() calls
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
			a, err := Ensure()
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
