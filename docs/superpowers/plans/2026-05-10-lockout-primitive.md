# Lockout Primitive Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add filesystem enforcement (chmod strip-write) under the existing tag system so locked files actually resist writes from cooperating Claudes and naive editors. Multi-file atomic acquire, best-effort release, and crash-recovery via doctor.

**Architecture:** A project-wide `flock` serializes lock/unlock operations. `AcquireLock` becomes multi-target and atomic — collect blockers, chmod-strip-write each target in canonical order, insert all rows, commit; rollback chmod on any failure. `ReleaseLock` becomes per-target best-effort, restoring owner-write. Lazy GC in `collectBlockers` chmod-restores stale rows. `doctor` flags orphan-mode files (stripped on disk, no DB row) but only repairs them under explicit `--restore-orphan-mode`.

**Tech Stack:** Go 1.22+, modernc.org/sqlite, syscall.Flock (unix-only via build tags). No new deps.

**Spec:** `docs/superpowers/specs/2026-05-10-lockout-primitive-design.md`

---

## Invariants

These must never be broken. If a task seems to require breaking one, stop and re-read the spec.

- The project op-flock — not the SQL transaction — is the true serialization boundary for `lock`/`unlock`. The DB tx exists only for atomic row visibility.
- Multi-target acquire is all-or-nothing: either every row is inserted and every file is stripped, or no rows remain and every prior chmod is restored best-effort with any restore failures surfaced (not swallowed).
- Unlock is best-effort per target. Atomicity belongs to acquire only.
- `state=no-lock` and `state=not-owner` are distinct outcomes (closes gh#46).
- Mode restoration is intentionally lossy: restore = `mode | 0o200` (owner-write). Pre-lock group/other-write bits are not preserved. Documented per spec §"chmod policy (no stored mode)".
- Doctor never restores orphan-mode files unless explicitly asked (`--restore-orphan-mode`). No silent dispossession of bytes.
- Lock targets are regular files only. Validation uses `os.Lstat` + `Mode().IsRegular()` so symlinks are rejected by their own type (not followed). Directories, symlinks, missing paths, and non-regular files are rejected with zero filesystem side effects.
- Hardlinked files (`st.Nlink > 1`) are rejected. Path-based DB rows cannot safely represent inode-level chmod state: if A locks `foo.go` and B locks `foo-alias.go` (same inode), A's unlock chmod-restores the shared inode while B's row still claims protection. The check is best-effort (TOCTOU between Lstat and chmod is possible) but catches the common case at zero cost.
- Store-level validation is mandatory inside `AcquireLocks` under the op-flock, not only at the CLI surface. CLI validation produces nice error output; the store is the primitive and must defend itself.
- Mailbox piggyback on break (NORTH_STAR invariant 8) is preserved.
- All target paths printed to stdout are cwd-relative when possible (per design.md).
- All chmod and DB mutations during acquire happen under the project op-flock. The DB tx may roll back; the op-flock guarantees no other `loto` process observes a half-stripped target set.

## NORTH_STAR conflict to reconcile

`docs/NORTH_STAR.md` lists `✗ multi-file atomic acquire (yet — YAGNI for now)` as a non-goal. This plan promotes it to first-class. A one-line doc-debt bead is filed alongside `loto-9ky` / `loto-qy6` in Task 18 step 4 to update NORTH_STAR's non-goals list with a sentence citing the use case (cooperating Claudes mid-sweep need atomic acquire across the changed file set, not per-file races).

## Out of scope, with forward pointers

- gh#45 (LOTO_AGENT_ID unset → identity collision) — tracked as `loto-200`. Boot.md flags as next trap; lockout enforcement is unaffected because identity is read once per `loto` invocation.
- Hook gate (`loto hook pre-write` from Claude PreToolUse) — unblocks once this lands.
- `loto with <cmd>` foreground flock wrapper — deferred.
- `original_mode` snapshot column — explicitly rejected by spec; lossy round-trip is the chosen trade.

---

## File Structure

**New files:**
- `internal/store/flock.go` — project op-flock acquire/release (`//go:build unix`). The flock file lives at `<state>/projects/<slug>/lock-op.flock`; its parent directory is the same one that holds `loto.db` and is created by the existing store-open path, so the flock helper can assume the parent exists.
- `internal/store/chmod.go` — `stripWrite` / `restoreWrite`; package-private `chmodFn` var for test injection
- `internal/render/cli.go` — multi-target output formatting per `design.md` (triage count first, key=value rows)
- `internal/render/cli_test.go`

**Modified files:**
- `internal/store/locks.go` — `AcquireLock([]LockRecord)`, `ReleaseLock([]Target)`; lazy GC chmod-restore
- `internal/store/doctor.go` — orphan-mode scan; `--restore-orphan-mode` repair path
- `internal/store/schema.sql` — bump `PRAGMA user_version`; add wipe-on-mismatch in `Open`
- `internal/store/store.go` — `user_version` check + `MoveCorruptAside`-on-mismatch
- `internal/cli/cmd_lock.go` — N positional args; multi-target atomic
- `internal/cli/cmd_unlock.go` — N positional args; per-target best-effort; distinct missing vs not-owner
- `internal/cli/cmd_break.go` — chmod-restore on `--force`
- `internal/cli/cmd_doctor.go` — wire `--restore-orphan-mode` flag
- `internal/cli/cmd_lock_test.go` — port single-target tests to multi-target shape
- `internal/cli/acceptance_test.go` — full acceptance suite

---

## Task 1: Schema bump + wipe-on-mismatch

**Files:**
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestOpen_WipesOnUserVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")

	// Create DB at a low user_version (simulates pre-lockout schema).
	db, err := sql.Open("sqlite", connDSN(path))
	if err != nil { t.Fatal(err) }
	if _, err := db.Exec(`PRAGMA user_version = 0`); err != nil { t.Fatal(err) }
	if _, err := db.Exec(`CREATE TABLE locks(target_canonical TEXT PRIMARY KEY)`); err != nil { t.Fatal(err) }
	if _, err := db.Exec(`INSERT INTO locks VALUES ('stale.go')`); err != nil { t.Fatal(err) }
	db.Close()

	s, err := Open(path)
	if err != nil { t.Fatalf("Open: %v", err) }
	defer s.Close()

	// Old row should be gone (DB was wiped).
	locks, err := s.ListLocks(context.Background())
	if err != nil { t.Fatal(err) }
	if len(locks) != 0 {
		t.Errorf("expected wiped DB, got %d locks", len(locks))
	}

	// Aside file should exist.
	matches, _ := filepath.Glob(path + ".corrupt.*")
	if len(matches) != 1 {
		t.Errorf("expected 1 aside file, got %d", len(matches))
	}
}
```

Add the missing `database/sql` and `path/filepath` imports if not already there.

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/store/ -run TestOpen_WipesOnUserVersionMismatch -v
```

Expected: FAIL — `Open` currently doesn't check `user_version`.

- [ ] **Step 3: Add user_version constant and check**

In `internal/store/schema.sql`, append at the end:

```sql
PRAGMA user_version = 3;
```

In `internal/store/store.go`, define a constant and check it in `openOnce`:

```go
const schemaUserVersion = 3
```

The check must distinguish "brand-new DB" from "existing DB at wrong version". A brand-new DB also starts at `user_version = 0` until `migrate()` runs schema.sql. Use `os.Stat` *before* `sql.Open` to decide. Refactor `Open`:

```go
func Open(p string) (*Store, error) {
	s, err := openOnce(p)
	if err == nil {
		return s, nil
	}
	if !isCorruptDB(err) && !isUserVersionMismatch(err) {
		return nil, err
	}
	moved, mvErr := MoveCorruptAside(p, time.Now())
	if mvErr != nil {
		return nil, fmt.Errorf("incompatible DB and move-aside failed: %w (orig: %w)", mvErr, err)
	}
	// Distinct messages for distinct conditions: schema mismatch vs byte damage.
	if isUserVersionMismatch(err) {
		fmt.Fprintf(os.Stderr, "loto: incompatible DB schema moved aside to %s; created fresh DB\n", moved)
	} else {
		fmt.Fprintf(os.Stderr, "loto: corrupt DB moved aside to %s; created fresh DB\n", moved)
	}
	return openOnce(p)
}
```

Add a sentinel + checker:

```go
var errUserVersionMismatch = errors.New("loto: schema user_version mismatch")

func isUserVersionMismatch(err error) bool { return errors.Is(err, errUserVersionMismatch) }
```

In `openOnce`, before `sql.Open`, capture whether the DB file pre-existed:

```go
preExisted := false
if st, err := os.Stat(p); err == nil && st.Size() > 0 {
	preExisted = true
}
```

After `db.PingContext` and *before* `s.migrate()`, read user_version. If the DB pre-existed and the version is not `schemaUserVersion` (including 0, which signals a pre-versioned DB), it's a mismatch — `migrate()` would silently bump version 0 → 3 over a foreign schema, so the check must run first:

```go
var v int
if err := db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
	db.Close()
	return nil, fmt.Errorf("read user_version: %w", err)
}
if preExisted && v != schemaUserVersion {
	db.Close()
	return nil, fmt.Errorf("%w: have %d, want %d", errUserVersionMismatch, v, schemaUserVersion)
}
```

Add `"errors"` import if missing. For brand-new DBs (`!preExisted`), version starts at 0; `migrate()` runs `schema.sql` which sets it to `schemaUserVersion`. Subsequent opens see the correct version.

- [ ] **Step 4: Run test to verify it passes**

```
go test ./internal/store/ -run TestOpen_WipesOnUserVersionMismatch -v
```

Expected: PASS.

- [ ] **Step 5: Run full store package tests**

```
go test ./internal/store/...
```

Expected: PASS. (No existing test creates a DB with a non-zero, non-matching user_version, so they should be unaffected.)

- [ ] **Step 6: Commit**

```bash
git add internal/store/schema.sql internal/store/store.go internal/store/store_test.go
git commit -m "store: bump user_version to 3, wipe DB on mismatch"
```

---

## Task 2: chmod helpers with injectable backend

**Files:**
- Create: `internal/store/chmod.go`
- Test: `internal/store/chmod_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/store/chmod_test.go`:

```go
package store

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStripWrite_RemovesAllWriteBits(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o664); err != nil { t.Fatal(err) }
	if err := stripWrite(p); err != nil { t.Fatal(err) }
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o222 != 0 {
		t.Errorf("expected no write bits, got %o", st.Mode().Perm())
	}
}

func TestRestoreWrite_AddsOwnerWrite(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil { t.Fatal(err) }
	if err := restoreWrite(p); err != nil { t.Fatal(err) }
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected owner write, got %o", st.Mode().Perm())
	}
}

func TestRestoreWrite_MissingFileIsNoop(t *testing.T) {
	if err := restoreWrite(filepath.Join(t.TempDir(), "nope")); err != nil {
		t.Errorf("missing file should be noop, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/store/ -run TestStripWrite -v
```

Expected: FAIL — `stripWrite`/`restoreWrite` undefined.

- [ ] **Step 3: Implement**

Create `internal/store/chmod.go`:

```go
package store

import (
	"errors"
	"io/fs"
	"os"
)

// chmodFn is a package-private indirection so tests can inject EPERM
// without an OS-specific fixture. See TestChmodRollback_FailureExits3.
var chmodFn = os.Chmod

// stripWrite removes all write bits (owner/group/other) from path.
// Returns the original mode before the strip so callers can roll back.
func stripWrite(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	return chmodFn(path, st.Mode().Perm()&^0o222)
}

// restoreWrite adds owner-write to path. Missing-file is a no-op
// (the file may have been deleted while held).
//
// restoreWrite intentionally restores ONLY owner-write (mode | 0o200).
// loto does not preserve exact pre-lock modes; a file at 0o400 round-trips
// to 0o600. Documented trade per spec §"chmod policy (no stored mode)".
func restoreWrite(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	return chmodFn(path, st.Mode().Perm()|0o200)
}
```

- [ ] **Step 4: Run tests to verify**

```
go test ./internal/store/ -run TestStripWrite -v
go test ./internal/store/ -run TestRestoreWrite -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/chmod.go internal/store/chmod_test.go
git commit -m "store: chmod helpers (stripWrite/restoreWrite) with injectable backend"
```

---

## Task 3: project op-flock helper

**Files:**
- Create: `internal/store/flock.go`
- Test: `internal/store/flock_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/store/flock_test.go`:

```go
//go:build unix

package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestOpFlock_SerializesConcurrentHolders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")

	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := acquireOpFlock(path, nil)
			if err != nil { t.Errorf("acquire: %v", err); return }
			defer h.release()
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
		}()
	}
	wg.Wait()

	if len(order) != 3 {
		t.Errorf("expected 3 holders, got %d", len(order))
	}
}
```

- [ ] **Step 2: Run to verify it fails**

```
go test ./internal/store/ -run TestOpFlock -v
```

Expected: FAIL — `acquireOpFlock` undefined.

- [ ] **Step 3: Implement**

Create `internal/store/flock.go`:

```go
//go:build unix

package store

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"time"
)

// ErrFlockTimeout is returned when acquireOpFlock cannot take the project
// op-flock within LOTO_FLOCK_TIMEOUT (default 30s).
var ErrFlockTimeout = errors.New("loto: op-flock acquire timed out")

const (
	flockPollInterval = 50 * time.Millisecond
	flockNoticeAfter  = 250 * time.Millisecond
	flockDefaultLimit = 30 * time.Second
)

type opFlock struct {
	f *os.File
}

func (h *opFlock) release() {
	if h == nil || h.f == nil { return }
	_ = syscall.Flock(int(h.f.Fd()), syscall.LOCK_UN)
	_ = h.f.Close()
}

// acquireOpFlock takes a project-wide exclusive flock on path with a bounded
// wait. Polls with LOCK_NB every 50ms; emits a one-shot wait notice on stderrW
// after 250ms cumulative wait; returns ErrFlockTimeout after LOTO_FLOCK_TIMEOUT
// (default 30s). Releases on process exit (kernel cleanup).
//
// stderrW is passed in (rather than read from a package global) so concurrent
// callers under `go test -race` cannot data-race on a shared writer.
func acquireOpFlock(path string, stderrW io.Writer) (*opFlock, error) {
	limit := flockDefaultLimit
	if s := os.Getenv("LOTO_FLOCK_TIMEOUT"); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d > 0 {
			limit = d
		}
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open op-flock: %w", err)
	}
	var noticed sync.Once
	deadline := time.Now().Add(limit)
	start := time.Now()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &opFlock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("flock op-flock: %w", err)
		}
		if stderrW != nil && time.Since(start) >= flockNoticeAfter {
			noticed.Do(func() { fmt.Fprintln(stderrW, "ℹ waiting flock=lock-op") })
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, ErrFlockTimeout
		}
		time.Sleep(flockPollInterval)
	}
}
```

The `Store` carries a stderr writer that callers (`AcquireLocks`, `ReleaseLocks`, `BreakLock`, doctor) pass into `acquireOpFlock`. Add to `Store`:

```go
type Store struct {
	db     *sql.DB
	dbPath string
	stderr io.Writer // defaults to os.Stderr; tests override via SetStderr
}

func (s *Store) SetStderr(w io.Writer) { s.stderr = w } // test-only injector
```

Initialize `stderr: os.Stderr` in `openOnce`. Add `"io"` import.

- [ ] **Step 4: Run test**

```
go test ./internal/store/ -run TestOpFlock -v
```

Expected: PASS.

- [ ] **Step 5: Add wait-notice test**

Append to `internal/store/flock_test.go`:

```go
func TestOpFlock_EmitsWaitNoticeAfter250ms(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")

	// Hold the flock from another goroutine for 400ms.
	h1, err := acquireOpFlock(path, nil)
	if err != nil { t.Fatal(err) }
	go func() {
		time.Sleep(400 * time.Millisecond)
		h1.release()
	}()

	var buf bytes.Buffer
	h2, err := acquireOpFlock(path, &buf)
	if err != nil { t.Fatalf("acquire: %v", err) }
	defer h2.release()

	if !strings.Contains(buf.String(), "waiting flock=lock-op") {
		t.Errorf("missing wait notice: %q", buf.String())
	}
}

func TestOpFlock_TimeoutAborts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock-op.flock")
	t.Setenv("LOTO_FLOCK_TIMEOUT", "100ms")

	h1, err := acquireOpFlock(path, nil)
	if err != nil { t.Fatal(err) }
	defer h1.release()

	start := time.Now()
	_, err = acquireOpFlock(path, nil)
	elapsed := time.Since(start)
	if !errors.Is(err, ErrFlockTimeout) {
		t.Fatalf("want ErrFlockTimeout, got %v", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("timeout took too long: %v", elapsed)
	}
}
```

Add `bytes`, `strings`, `errors` imports. Run: `go test ./internal/store/ -run TestOpFlock -v`. Expected: PASS (both wait-notice and timeout tests).

- [ ] **Step 6: Commit**

```bash
git add internal/store/flock.go internal/store/flock_test.go
git commit -m "store: project op-flock helper (unix-only)"
```

---

## Task 4: Store-level op-flock path resolution

**Files:**
- Modify: `internal/store/store.go`
- Test: `internal/store/store_test.go`

The op-flock lives at `<state>/projects/<slug>/lock-op.flock`. The store needs to know its state directory so it can place the flock file alongside `loto.db`.

- [ ] **Step 1: Write the failing test**

Add to `internal/store/store_test.go`:

```go
func TestStore_OpFlockPathDerivedFromDBPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "loto.db")
	s, err := Open(path)
	if err != nil { t.Fatal(err) }
	defer s.Close()
	if got := s.opFlockPath(); got != filepath.Join(dir, "lock-op.flock") {
		t.Errorf("opFlockPath = %q, want %q", got, filepath.Join(dir, "lock-op.flock"))
	}
}
```

- [ ] **Step 2: Run test**

```
go test ./internal/store/ -run TestStore_OpFlockPath -v
```

Expected: FAIL.

- [ ] **Step 3: Wire path through Store**

In `internal/store/store.go`, store the dbPath on the Store struct:

```go
type Store struct {
	db     *sql.DB
	dbPath string
}
```

Update `openOnce` to set `s := &Store{db: db, dbPath: p}`.

Add the method:

```go
func (s *Store) opFlockPath() string {
	return filepath.Join(filepath.Dir(s.dbPath), "lock-op.flock")
}
```

Add `"path/filepath"` import if not present.

- [ ] **Step 4: Run test**

```
go test ./internal/store/ -run TestStore_OpFlockPath -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/store.go internal/store/store_test.go
git commit -m "store: expose opFlockPath() derived from db path"
```

---

## Task 5: Multi-target AcquireLock (atomic chmod + rollback)

**Files:**
- Modify: `internal/store/locks.go`
- Test: `internal/store/locks_test.go`

This is the heart of the change. `AcquireLock` becomes a multi-target atomic operation: collect blockers across all targets, chmod-strip-write each in canonical order, insert all rows in one tx, rollback chmod on any failure.

- [ ] **Step 1: Write failing test for happy path**

Add to `internal/store/locks_test.go`:

```go
func TestAcquireLocks_MultiFile_AtomicSuccess(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go"); b := filepath.Join(dir, "b.go")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	if err := os.WriteFile(b, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	s := openTestStore(t)
	defer s.Close()

	now := time.Now()
	recs := []domain.LockRecord{
		{Target: domain.Target{Canonical: a, Kind: domain.KindFile}, OwnerUUID: "agent1", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
		{Target: domain.Target{Canonical: b, Kind: domain.KindFile}, OwnerUUID: "agent1", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
	}
	live := func(string, int) bool { return true }

	if _, err := s.AcquireLocks(context.Background(), recs, live); err != nil {
		t.Fatalf("AcquireLocks: %v", err)
	}

	for _, p := range []string{a, b} {
		st, _ := os.Stat(p)
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s: expected stripped write, got %o", p, st.Mode().Perm())
		}
	}
}
```

You'll need a helper `openTestStore`:

```go
func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "loto.db"))
	if err != nil { t.Fatal(err) }
	return s
}
```

(If a similar helper already exists, reuse it — check `internal/store/locks_test.go` first.)

- [ ] **Step 2: Run test to verify it fails**

```
go test ./internal/store/ -run TestAcquireLocks_MultiFile_AtomicSuccess -v
```

Expected: FAIL — `AcquireLocks` undefined.

- [ ] **Step 3: Add MultiConflictError and chmod-failure types**

In `internal/store/locks.go`, add:

```go
// MultiConflictError aggregates blockers across multiple targets.
type MultiConflictError struct {
	Blockers []domain.LockRecord // sorted: created_at asc, then canonical asc
}

func (e *MultiConflictError) Error() string {
	return fmt.Sprintf("multi-target lock conflict: %d blocker(s)", len(e.Blockers))
}

// ChmodFailure describes a single target's chmod outcome during a failed
// multi-acquire. RolledBack=true means the strip was successfully reversed.
// Err is the underlying os.Chmod error (not a syscall errno).
type ChmodFailure struct {
	Target     domain.Target
	Err        error
	RolledBack bool // true if subsequent restoreWrite succeeded
}

type ChmodFailureError struct {
	Failures []ChmodFailure
}

func (e *ChmodFailureError) Error() string {
	return fmt.Sprintf("chmod failed on %d target(s)", len(e.Failures))
}
```

- [ ] **Step 4: Implement AcquireLocks**

Add to `internal/store/locks.go`:

```go
// AcquireLocks atomically acquires locks on all targets. Either all targets
// are stripped-write + DB rows inserted, or none are (with chmod rollback).
//
// If the process dies between the chmod loop and tx.Commit, files are stripped
// with no DB row — exactly the orphan-mode case `doctor` is designed to surface.
//
// Errors:
//   *MultiConflictError — one or more targets blocked; no side effects
//   *ChmodFailureError  — chmod failed mid-op; rollback attempted, may have
//                         left mode_restore_failed system tags
//   other               — internal/SQL error; tx aborted
func (s *Store) AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error) {
	if len(recs) == 0 {
		return nil, nil
	}

	// Canonical sort for deterministic order (rollback must be reverse-of-acquire).
	sorted := make([]domain.LockRecord, len(recs))
	copy(sorted, recs)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Target.Canonical < sorted[j].Target.Canonical
	})

	flock, err := acquireOpFlock(s.opFlockPath(), s.stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()

	// Defense-in-depth: validate each target at the store layer too. The CLI
	// already filters but the store is the primitive — it must defend itself.
	// Lstat (not Stat) so symlinks are visible; Nlink > 1 rejects hardlinks
	// whose shared inode breaks path-based chmod state. Best-effort: a TOCTOU
	// race between Lstat and chmod is possible and accepted.
	for i := range sorted {
		p := sorted[i].Target.Canonical
		lst, lerr := os.Lstat(p)
		if lerr != nil {
			return nil, fmt.Errorf("validate %s: %w", p, lerr)
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("validate %s: symlink not supported", p)
		}
		if !lst.Mode().IsRegular() {
			return nil, fmt.Errorf("validate %s: not a regular file", p)
		}
		if sys, ok := lst.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
			return nil, fmt.Errorf("validate %s: hardlinked file (Nlink=%d) not supported", p, sys.Nlink)
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	caseSensitive, err := s.fsCaseSensitiveTx(tx)
	if err != nil { return nil, err }
	caseInsensitive := !caseSensitive

	all, err := loadLocksTx(ctx, tx)
	if err != nil { return nil, err }
	now := time.Now()

	// Collect blockers across ALL targets first.
	var blockers []domain.LockRecord
	for i := range sorted {
		bs, err := collectBlockers(ctx, tx, all, sorted[i], caseInsensitive, now, live)
		if err != nil { return nil, err }
		blockers = append(blockers, bs...)
	}
	if len(blockers) > 0 {
		sort.Slice(blockers, func(i, j int) bool {
			if !blockers[i].CreatedAt.Equal(blockers[j].CreatedAt) {
				return blockers[i].CreatedAt.Before(blockers[j].CreatedAt)
			}
			return blockers[i].Target.Canonical < blockers[j].Target.Canonical
		})
		return nil, &MultiConflictError{Blockers: blockers}
	}

	// Chmod each target. On first failure, roll back prior strips. Restore
	// failures are *collected* in memory here; we cannot durably record them
	// while the acquire tx is still open (deferred rollback would race with
	// any in-tx insert against the same connection), so the system-tag write
	// happens AFTER tx.Rollback() and BEFORE flock.release().
	stripped := make([]string, 0, len(sorted))
	for i := range sorted {
		path := sorted[i].Target.Canonical
		if err := stripWrite(path); err != nil {
			failures := []ChmodFailure{{Target: sorted[i].Target, Err: err, RolledBack: false}}
			var restoreErrs []chmodRestoreErr // collected; written after rollback
			for _, p := range stripped {
				if rbErr := restoreWrite(p); rbErr != nil {
					restoreErrs = append(restoreErrs, chmodRestoreErr{path: p, err: rbErr})
					failures = append(failures, ChmodFailure{
						Target: domain.Target{Canonical: p, Kind: domain.KindFile},
						Err: rbErr, RolledBack: false,
					})
				} else {
					failures = append(failures, ChmodFailure{
						Target: domain.Target{Canonical: p, Kind: domain.KindFile},
						RolledBack: true,
					})
				}
			}
			// Roll back the acquire tx FIRST, then write the breadcrumb tags
			// using a fresh tx on s.db (still under the op-flock).
			_ = tx.Rollback()
			for _, re := range restoreErrs {
				_ = s.appendModeRestoreFailedTag(ctx, re.path, sorted[0].OwnerUUID, now, re.err)
			}
			return nil, &ChmodFailureError{Failures: failures}
		}
		stripped = append(stripped, path)
	}

	// Insert all rows.
	for i := range sorted {
		if err := insertOrRefreshLock(ctx, tx, sorted[i]); err != nil {
			// Roll back chmod for everything we stripped.
			for _, p := range stripped { _ = restoreWrite(p) }
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		for _, p := range stripped { _ = restoreWrite(p) }
		return nil, err
	}
	return sorted, nil
}

// chmodRestoreErr buffers a per-target restore failure so it can be turned
// into a durable mode_restore_failed tag AFTER the acquire tx rolls back.
type chmodRestoreErr struct {
	path string
	err  error
}

// appendModeRestoreFailedTag writes the durable breadcrumb on its own
// connection. Callers MUST have rolled back the surrounding acquire tx
// first; writing while that tx is still open against the same *sql.DB
// risks SQLITE_BUSY / connection-pool contention.
func (s *Store) appendModeRestoreFailedTag(ctx context.Context, path, byAgent string, now time.Time, cause error) error {
	tagID := newTagID(byAgent, now, "mode_restore_failed")
	_, err := s.db.ExecContext(ctx, `
INSERT INTO tags(target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		path, "file", tagID, "system", "mode_restore_failed",
		byAgent, byAgent, "",
		fmt.Sprintf("mode_restore_failed: %v on %s", cause, path),
		now.UnixNano(),
	)
	return err
}
```

**Delete** the existing single-target `AcquireLock` and the `ConflictError` type. Callers (cmd_lock.go) move to `AcquireLocks` / `MultiConflictError` in Task 11. Any test in `internal/store/locks_test.go` referencing `AcquireLock` or `ConflictError` gets ported to the multi-target API in this same task — search-and-replace, then read each call site to confirm it still expresses the test's intent.

Run `grep -rn "AcquireLock\b\|ConflictError\b" internal/` after deletion. Every hit must be either the new `AcquireLocks` / `MultiConflictError` or in a file you're about to rewrite in Task 11. No wrapper, no shim, no "back-compat through this PR" code.

- [ ] **Step 5: Run happy-path test**

```
go test ./internal/store/ -run TestAcquireLocks_MultiFile_AtomicSuccess -v
```

Expected: PASS.

- [ ] **Step 6: Add conflict-aborts test**

Append to `internal/store/locks_test.go`:

```go
func TestAcquireLocks_MultiFile_ConflictAbortsNoChmod(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go"); b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	s := openTestStore(t); defer s.Close()
	live := func(string, int) bool { return true }
	now := time.Now()
	mk := func(target, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target: domain.Target{Canonical: target, Kind: domain.KindFile},
			OwnerUUID: owner, SessionUUID: owner,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			Host: "h", PID: 1,
		}
	}

	// agent1 already holds a.go.
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{mk(a, "agent1")}, live); err != nil {
		t.Fatal(err)
	}
	// File a.go is now stripped. Restore for clarity of the test:
	stA, _ := os.Stat(a); modeABefore := stA.Mode().Perm()
	stB, _ := os.Stat(b); modeBBefore := stB.Mode().Perm()

	// agent2 tries to acquire BOTH. Should fail with conflict, no chmod side effect on b.
	_, err := s.AcquireLocks(context.Background(), []domain.LockRecord{mk(a, "agent2"), mk(b, "agent2")}, live)
	if err == nil { t.Fatal("expected conflict, got nil") }
	var mce *MultiConflictError
	if !errors.As(err, &mce) { t.Fatalf("want *MultiConflictError, got %T", err) }

	stA2, _ := os.Stat(a)
	if stA2.Mode().Perm() != modeABefore {
		t.Errorf("a.go mode changed: %o → %o", modeABefore, stA2.Mode().Perm())
	}
	stB2, _ := os.Stat(b)
	if stB2.Mode().Perm() != modeBBefore {
		t.Errorf("b.go mode changed: %o → %o (should be untouched)", modeBBefore, stB2.Mode().Perm())
	}
}
```

- [ ] **Step 7: Run conflict test**

```
go test ./internal/store/ -run TestAcquireLocks_MultiFile_ConflictAbortsNoChmod -v
```

Expected: PASS.

- [ ] **Step 8: Add chmod-rollback test using injected EPERM**

Append:

```go
func TestAcquireLocks_ChmodFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go"); b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	s := openTestStore(t); defer s.Close()

	// Inject: chmod succeeds on a.go, fails on b.go.
	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		if path == b {
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}

	live := func(string, int) bool { return true }
	now := time.Now()
	recs := []domain.LockRecord{
		{Target: domain.Target{Canonical: a, Kind: domain.KindFile}, OwnerUUID: "agent1", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
		{Target: domain.Target{Canonical: b, Kind: domain.KindFile}, OwnerUUID: "agent1", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
	}
	_, err := s.AcquireLocks(context.Background(), recs, live)
	var cfe *ChmodFailureError
	if !errors.As(err, &cfe) { t.Fatalf("want *ChmodFailureError, got %v", err) }

	// a.go must be restored (write bit on).
	stA, _ := os.Stat(a)
	if stA.Mode().Perm()&0o200 == 0 {
		t.Errorf("a.go not restored: %o", stA.Mode().Perm())
	}
	// No DB rows created.
	locks, _ := s.ListLocks(context.Background())
	if len(locks) != 0 {
		t.Errorf("expected 0 locks, got %d", len(locks))
	}
}
```

- [ ] **Step 8b: Add rollback-restore-failure test (covers mode_restore_failed tag)**

The most useful diagnostic breadcrumb for a partial-strip crash is the `mode_restore_failed` system tag. Inject failures so the rollback path also fails its own restore:

```go
func TestAcquireLocks_RollbackRestoreFailureLeavesBreadcrumb(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go"); b := filepath.Join(dir, "b.go")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	s := openTestStore(t); defer s.Close()

	// Strip succeeds on a.go, fails on b.go; then restore on a.go also fails.
	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		switch {
		case path == b: // strip on b fails
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		case path == a && mode.Perm()&0o200 != 0: // restore on a also fails
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}

	live := func(string, int) bool { return true }
	now := time.Now()
	recs := []domain.LockRecord{
		{Target: domain.Target{Canonical: a, Kind: domain.KindFile}, OwnerUUID: "alice", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
		{Target: domain.Target{Canonical: b, Kind: domain.KindFile}, OwnerUUID: "alice", SessionUUID: "s1", CreatedAt: now, ExpiresAt: now.Add(time.Hour), Host: "h", PID: 1},
	}
	_, err := s.AcquireLocks(context.Background(), recs, live)
	var cfe *ChmodFailureError
	if !errors.As(err, &cfe) { t.Fatalf("want *ChmodFailureError, got %v", err) }

	// a.go's restore failed → ChmodFailure for a has RolledBack=false.
	var aFailure *ChmodFailure
	for i := range cfe.Failures {
		if cfe.Failures[i].Target.Canonical == a { aFailure = &cfe.Failures[i] }
	}
	if aFailure == nil || aFailure.RolledBack {
		t.Fatalf("expected a.go failure with RolledBack=false, got %+v", aFailure)
	}

	// mode_restore_failed tag exists in the DB for a.go.
	var n int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE target_canonical=? AND event='mode_restore_failed'`, a,
	).Scan(&n); err != nil { t.Fatal(err) }
	if n != 1 {
		t.Errorf("want 1 mode_restore_failed tag for %s, got %d", a, n)
	}
}
```

- [ ] **Step 9: Run all locks tests**

```
go test ./internal/store/ -run TestAcquireLocks -v
```

Expected: all PASS.

- [ ] **Step 10: Stage but defer commit**

Stage the store changes but do NOT commit yet — deleting `AcquireLock`/`ConflictError` here without rewriting `cmd_lock.go` (Task 11) breaks `go build` between commits, ruining `git bisect`. Combined commit lands in Task 11 step 6.

```bash
git add internal/store/locks.go internal/store/locks_test.go
# defer git commit until Task 11 step 6
```

---

## Task 6: Multi-target ReleaseLock with distinct missing/not-owner

**Files:**
- Modify: `internal/store/locks.go`
- Test: `internal/store/locks_test.go`

Per spec: best-effort, per-target. Returns a structured per-target outcome so the CLI can render rows per `design.md`. Folds gh#46.

- [ ] **Step 1: Write failing test**

Add to `internal/store/locks_test.go`:

```go
func TestReleaseLocks_DistinguishesMissingFromNotOwner(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.go"); b := filepath.Join(dir, "b.go"); c := filepath.Join(dir, "c.go")
	for _, p := range []string{a, b, c} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	s := openTestStore(t); defer s.Close()
	live := func(string, int) bool { return true }
	now := time.Now()

	mk := func(target, owner string) domain.LockRecord {
		return domain.LockRecord{
			Target: domain.Target{Canonical: target, Kind: domain.KindFile},
			OwnerUUID: owner, SessionUUID: owner,
			CreatedAt: now, ExpiresAt: now.Add(time.Hour),
			Host: "h", PID: 1,
		}
	}
	// alice locks a, bob locks c.
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{mk(a, "alice")}, live); err != nil { t.Fatal(err) }
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{mk(c, "bob")}, live); err != nil { t.Fatal(err) }

	// alice tries to release a, b, c.
	results, err := s.ReleaseLocks(context.Background(), []domain.Target{
		{Canonical: a, Kind: domain.KindFile},
		{Canonical: b, Kind: domain.KindFile},
		{Canonical: c, Kind: domain.KindFile},
	}, "alice")
	if err != nil { t.Fatalf("ReleaseLocks: %v", err) }

	if len(results) != 3 { t.Fatalf("want 3 results, got %d", len(results)) }
	want := []ReleaseOutcome{StateUnlocked, StateNoLock, StateNotOwner}
	for i, r := range results {
		if r.State != want[i] {
			t.Errorf("results[%d].State = %v, want %v", i, r.State, want[i])
		}
	}
	// a.go should be restored, c.go still stripped.
	stA, _ := os.Stat(a)
	if stA.Mode().Perm()&0o200 == 0 { t.Errorf("a.go not restored") }
	stC, _ := os.Stat(c)
	if stC.Mode().Perm()&0o222 != 0 { t.Errorf("c.go should remain stripped") }
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/store/ -run TestReleaseLocks_DistinguishesMissingFromNotOwner -v
```

Expected: FAIL.

- [ ] **Step 3: Implement**

In `internal/store/locks.go`:

```go
type ReleaseOutcome int

const (
	StateUnlocked ReleaseOutcome = iota
	StateNoLock
	StateNotOwner
	StateRestoreFailed // row deleted, chmod restore failed
)

type ReleaseResult struct {
	Target     domain.Target
	State      ReleaseOutcome
	Holder     string // populated when State == StateNotOwner
	RestoreErr error  // populated when State == StateRestoreFailed
}

// ReleaseLocks releases each target best-effort.
// Returns one ReleaseResult per input target in input order — the op-flock
// serializes the actual work; render does the canonical sort for stable
// output. Errors only on internal/SQL failures.
func (s *Store) ReleaseLocks(ctx context.Context, targets []domain.Target, byAgent string) ([]ReleaseResult, error) {
	if len(targets) == 0 { return nil, nil }

	flock, err := acquireOpFlock(s.opFlockPath(), s.stderr)
	if err != nil { return nil, err }
	defer flock.release()

	results := make([]ReleaseResult, 0, len(targets))
	for _, t := range targets {
		r, err := s.releaseOne(ctx, t, byAgent)
		if err != nil { return nil, err }
		results = append(results, r)
	}
	return results, nil
}

func (s *Store) releaseOne(ctx context.Context, t domain.Target, byAgent string) (ReleaseResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil { return ReleaseResult{}, err }
	defer func() { _ = tx.Rollback() }()

	var owner string
	err = tx.QueryRowContext(ctx, `SELECT owner_uuid FROM locks WHERE target_canonical = ?`, t.Canonical).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ReleaseResult{Target: t, State: StateNoLock}, nil
	}
	if err != nil { return ReleaseResult{}, err }
	if owner != byAgent {
		return ReleaseResult{Target: t, State: StateNotOwner, Holder: owner}, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, t.Canonical, byAgent); err != nil {
		return ReleaseResult{}, err
	}
	if err := tx.Commit(); err != nil { return ReleaseResult{}, err }

	// Chmod restore is outside the tx — surface failure but the lock IS released.
	if rerr := restoreWrite(t.Canonical); rerr != nil {
		return ReleaseResult{Target: t, State: StateRestoreFailed, RestoreErr: rerr}, nil
	}
	return ReleaseResult{Target: t, State: StateUnlocked}, nil
}
```

- [ ] **Step 3b: Restore-failure test**

Append to `internal/store/locks_test.go`:

```go
func TestRelease_RestoreFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	s := openTestStore(t); defer s.Close()
	live := func(string, int) bool { return true }
	now := time.Now()
	rec := domain.LockRecord{
		Target: domain.Target{Canonical: p, Kind: domain.KindFile},
		OwnerUUID: "alice", SessionUUID: "alice",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Host: "h", PID: 1,
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live); err != nil { t.Fatal(err) }

	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		if path == p && mode.Perm()&0o200 != 0 {
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}

	results, err := s.ReleaseLocks(context.Background(), []domain.Target{rec.Target}, "alice")
	if err != nil { t.Fatal(err) }
	if len(results) != 1 || results[0].State != StateRestoreFailed {
		t.Fatalf("want StateRestoreFailed, got %+v", results)
	}
	if results[0].RestoreErr == nil { t.Error("RestoreErr nil") }
}
```

**Delete** the existing `ReleaseLock`. Callers (cmd_unlock.go, `unlockAllMine`, any test) move to `ReleaseLocks` returning `[]ReleaseResult` in Task 12 — no wrapper. Run `grep -rn "ReleaseLock\b" internal/` after deletion; every hit must be `ReleaseLocks` or in a file you're rewriting in Task 12.

- [ ] **Step 4: Run tests**

```
go test ./internal/store/ -v
```

Expected: PASS (existing tests + new ReleaseLocks test).

- [ ] **Step 5: Stage but defer commit**

Same bisect-cleanliness rationale as Task 5 — deleting `ReleaseLock` here without rewriting `cmd_unlock.go` (Task 12) breaks the build between commits. Combined commit lands in Task 12 step 5.

```bash
git add internal/store/locks.go internal/store/locks_test.go
# defer git commit until Task 12 step 5
```

---

## Task 7: Lazy GC chmod-restore in collectBlockers

**Files:**
- Modify: `internal/store/locks.go`
- Test: `internal/store/locks_test.go`

When `collectBlockers` reclaims a stale row via `reclaimStaleTx`, also restore write on the file. Per spec: side-effect asymmetry is acknowledged.

- [ ] **Step 1: Write failing test**

The reclaim's chmod-restore must fire *before* the new acquire's strip. Asserting on the final file mode alone is ambiguous (strip-then-strip looks identical to restore-then-strip). Spy on `chmodFn` and assert the call sequence on the reclaimed path: first call restores owner-write, second call strips.

```go
func TestReclaimStale_RestoresWriteMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	s := openTestStore(t); defer s.Close()
	now := time.Now()

	// Seed a stale (expired + dead PID) lock row, with the file already stripped.
	if err := stripWrite(p); err != nil { t.Fatal(err) }
	rec := domain.LockRecord{
		Target: domain.Target{Canonical: p, Kind: domain.KindFile},
		OwnerUUID: "ghost", SessionUUID: "ghost",
		CreatedAt: now.Add(-2 * time.Hour),
		ExpiresAt: now.Add(-time.Hour),
		Host: "h", PID: 1,
	}
	tx, _ := s.db.Begin()
	if err := insertOrRefreshLock(context.Background(), tx, rec); err != nil { t.Fatal(err) }
	if err := tx.Commit(); err != nil { t.Fatal(err) }

	// Spy on chmodFn to capture the call sequence on p.
	orig := chmodFn
	defer func() { chmodFn = orig }()
	var calls []os.FileMode
	chmodFn = func(path string, mode os.FileMode) error {
		if path == p { calls = append(calls, mode.Perm()) }
		return orig(path, mode)
	}

	// agent2 acquires the same path; collectBlockers should reclaim, restoring
	// write, and then the new acquire's stripWrite runs.
	live := func(string, int) bool { return false }
	newRec := domain.LockRecord{
		Target: rec.Target,
		OwnerUUID: "agent2", SessionUUID: "agent2",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Host: "h", PID: 2,
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{newRec}, live); err != nil {
		t.Fatalf("acquire: %v", err)
	}

	if len(calls) < 2 {
		t.Fatalf("expected ≥2 chmod calls on %s, got %d", p, len(calls))
	}
	if calls[0]&0o200 == 0 {
		t.Errorf("first chmod should restore owner-write, got %o", calls[0])
	}
	if calls[1]&0o222 != 0 {
		t.Errorf("second chmod should strip write, got %o", calls[1])
	}
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/store/ -run TestReclaimStale_RestoresWriteMode -v
```

Expected: FAIL — current `reclaimStaleTx` doesn't call `restoreWrite`, so only one chmod call appears.

- [ ] **Step 3: Implement**

In `internal/store/locks.go`, modify `reclaimStaleTx` to call `restoreWrite` after the DELETE:

```go
func reclaimStaleTx(ctx context.Context, tx *sql.Tx, stale domain.LockRecord, byAgent string, now time.Time) error {
	tagID := newTagID(byAgent, now, "lock_reclaimed_stale")
	if _, err := tx.ExecContext(ctx, `
INSERT INTO tags(target_canonical,target_kind,id,kind,event,author_uuid,addressee_uuid,previous_owner_uuid,intent,created_at,expires_at)
VALUES (?,?,?,?,?,?,?,?,?,?,NULL)`,
		stale.Target.Canonical, kindString(stale.Target.Kind), tagID, "system", "lock_reclaimed_stale",
		byAgent, stale.OwnerUUID, stale.OwnerUUID,
		"reclaimed stale lock", now.UnixNano(),
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, stale.Target.Canonical, stale.OwnerUUID); err != nil {
		return err
	}
	// Lazy GC chmod-restore. Side-effect asymmetry: if the surrounding tx
	// later aborts on a different target, this restore is not undone.
	// Acceptable per spec — stale-and-dead rows aren't protecting anyone.
	_ = restoreWrite(stale.Target.Canonical)
	return nil
}
```

- [ ] **Step 4: Run test**

```
go test ./internal/store/ -run TestReclaimStale_RestoresWriteMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/locks.go internal/store/locks_test.go
git commit -m "store: lazy GC restores write mode on stale row reclaim"
```

---

## Task 8: BreakLock chmod restore

**Files:**
- Modify: `internal/store/locks.go`
- Test: `internal/store/locks_test.go`

`BreakLock` deletes a lock row but currently leaves the file stripped. Restore on success (whether `--force` or natural reclaim).

- [ ] **Step 1: Write failing test**

```go
func TestBreakLock_RestoresWriteMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	s := openTestStore(t); defer s.Close()
	live := func(string, int) bool { return true }
	now := time.Now()
	rec := domain.LockRecord{
		Target: domain.Target{Canonical: p, Kind: domain.KindFile},
		OwnerUUID: "alice", SessionUUID: "alice",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Host: "h", PID: 1,
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live); err != nil { t.Fatal(err) }

	// bob force-breaks alice's lock.
	if err := s.BreakLock(context.Background(), rec.Target, "bob", true, "test", live); err != nil {
		t.Fatalf("BreakLock: %v", err)
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored, got %o", st.Mode().Perm())
	}
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/store/ -run TestBreakLock_RestoresWriteMode -v
```

Expected: FAIL.

- [ ] **Step 3: Implement**

`BreakLock` mutates both a lock row AND chmod state, so it must serialize under the same project op-flock as acquire/release. Take the flock at function entry. Then, after `tx.Commit()`, restore write mode. Concretely:

1. At the top of `BreakLock`, before `tx, err := s.db.BeginTx(...)`, add:

```go
flock, err := acquireOpFlock(s.opFlockPath(), s.stderr)
if err != nil { return err }
defer flock.release()
```

2. Change the tail of `BreakLock` from:

```go
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, t.Canonical, l.OwnerUUID); err != nil {
		return err
	}
	return tx.Commit()
}
```

to:

```go
	if _, err := tx.ExecContext(ctx, `DELETE FROM locks WHERE target_canonical = ? AND owner_uuid = ?`, t.Canonical, l.OwnerUUID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil { return err }
	if rerr := restoreWrite(t.Canonical); rerr != nil {
		return &RestoreFailedError{Path: t.Canonical, Inner: rerr}
	}
	return nil
}
```

Also add to `internal/store/locks.go`:

```go
// RestoreFailedError signals that a destructive operation (break/release) freed
// the lock row successfully but failed to chmod the file back to writable.
// The lock IS released; the caller should still render the success path
// alongside a warning.
type RestoreFailedError struct {
	Path  string
	Inner error
}

func (e *RestoreFailedError) Error() string {
	return fmt.Sprintf("restore-failed target=%s err=%v", e.Path, e.Inner)
}
func (e *RestoreFailedError) Unwrap() error { return e.Inner }
```

Add a CLI-side test (Task 13 will exercise this via the break surface):

```go
func TestBreak_RestoreFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	s := openTestStore(t); defer s.Close()
	live := func(string, int) bool { return true }
	now := time.Now()
	rec := domain.LockRecord{
		Target: domain.Target{Canonical: p, Kind: domain.KindFile},
		OwnerUUID: "alice", SessionUUID: "alice",
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
		Host: "h", PID: 1,
	}
	if _, err := s.AcquireLocks(context.Background(), []domain.LockRecord{rec}, live); err != nil { t.Fatal(err) }

	orig := chmodFn
	defer func() { chmodFn = orig }()
	chmodFn = func(path string, mode os.FileMode) error {
		if path == p && mode.Perm()&0o200 != 0 {
			return &os.PathError{Op: "chmod", Path: path, Err: syscall.EPERM}
		}
		return orig(path, mode)
	}
	err := s.BreakLock(context.Background(), rec.Target, "bob", true, "stuck", live)
	var rfe *RestoreFailedError
	if !errors.As(err, &rfe) {
		t.Fatalf("want *RestoreFailedError, got %v", err)
	}
}
```

- [ ] **Step 4: Run test**

```
go test ./internal/store/ -run TestBreakLock_RestoresWriteMode -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store/locks.go internal/store/locks_test.go
git commit -m "store: BreakLock restores write mode after delete"
```

---

## Task 9: Doctor orphan-mode scan + --restore-orphan-mode

**Files:**
- Modify: `internal/store/doctor.go`
- Modify: `internal/cli/cmd_doctor.go`
- Test: `internal/store/doctor_test.go`
- Test: `internal/cli/cmd_doctor_test.go`

Orphan-mode = file is stripped on disk but no DB row owns it. Default `--repair` does NOT touch these (no silent dispossession of bytes). Explicit `--restore-orphan-mode` does.

- [ ] **Step 1: Write failing test**

Add to `internal/store/doctor_test.go`:

```go
func TestDoctorAudit_DetectsOrphanModeFiles(t *testing.T) {
	dir := t.TempDir()
	orphan := filepath.Join(dir, "orphan.go")
	clean := filepath.Join(dir, "clean.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil { t.Fatal(err) }
	if err := os.WriteFile(clean, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	s := openTestStore(t); defer s.Close()

	orphans, err := s.ScanOrphanModes(context.Background(), []string{orphan, clean})
	if err != nil { t.Fatal(err) }
	if len(orphans) != 1 || orphans[0] != orphan {
		t.Errorf("orphans = %v, want [%s]", orphans, orphan)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/store/ -run TestDoctorAudit_DetectsOrphanModeFiles -v
```

Expected: FAIL — `ScanOrphanModes` undefined.

- [ ] **Step 3: Implement scanner**

In `internal/store/doctor.go`:

```go
// ScanOrphanModes returns paths that are read-only on disk but have no
// matching lock row. Caller supplies the candidate paths (typically all
// regular files under the repo, or a curated subset).
func (s *Store) ScanOrphanModes(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 { return nil, nil }
	rows, err := s.db.QueryContext(ctx, `SELECT target_canonical FROM locks`)
	if err != nil { return nil, err }
	defer rows.Close()
	owned := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil { return nil, err }
		owned[c] = true
	}
	if err := rows.Err(); err != nil { return nil, err }

	var orphans []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil { continue }
		if !st.Mode().IsRegular() { continue }
		if st.Mode().Perm()&0o222 != 0 { continue } // writable: not stripped
		if owned[p] { continue }
		orphans = append(orphans, p)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// RestoreOrphanMode chmods the given paths back to owner-writable. Caller
// gates this behind explicit user intent (--restore-orphan-mode).
func (s *Store) RestoreOrphanMode(paths []string) []string {
	var restored []string
	for _, p := range paths {
		if err := restoreWrite(p); err == nil {
			restored = append(restored, p)
		}
	}
	return restored
}
```

Add `"sort"` import if missing.

- [ ] **Step 4: Run test**

```
go test ./internal/store/ -run TestDoctorAudit_DetectsOrphanModeFiles -v
```

Expected: PASS.

- [ ] **Step 5: Wire CLI flag (failing test first)**

Add to `internal/cli/cmd_doctor_test.go`:

```go
func TestDoctor_OrphanModeFlaggedNotRepaired(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil { t.Fatal(err) }

	var out bytes.Buffer
	// --orphan-mode reports but does not repair; --repair alone does not walk.
	code := Run([]string{"doctor", "--repair", "--orphan-mode"}, &out, io.Discard)
	if code != 0 { t.Fatalf("exit %d: %s", code, out.String()) }

	// Orphan should still be 0o444 (--orphan-mode reports, --restore-orphan-mode repairs).
	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 != 0 {
		t.Errorf("orphan unexpectedly restored: %o", st.Mode().Perm())
	}
	if !strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("expected orphan-mode in output: %s", out.String())
	}
}

func TestDoctor_DefaultDoesNotWalkTree(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil { t.Fatal(err) }

	var out bytes.Buffer
	code := Run([]string{"doctor"}, &out, io.Discard)
	if code != 0 { t.Fatalf("exit %d: %s", code, out.String()) }
	if strings.Contains(out.String(), "orphan-mode") {
		t.Errorf("default doctor should not walk: %s", out.String())
	}
}

func TestDoctor_RestoreOrphanModeFlagRepairs(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	orphan := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(orphan, []byte("x"), 0o444); err != nil { t.Fatal(err) }

	var out bytes.Buffer
	code := Run([]string{"doctor", "--repair", "--restore-orphan-mode"}, &out, io.Discard)
	if code != 0 { t.Fatalf("exit %d: %s", code, out.String()) }

	st, _ := os.Stat(orphan)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored, got %o", st.Mode().Perm())
	}
}
```

(Add `path/filepath`, `os`, `io`, `bytes`, `strings` imports as needed.)

- [ ] **Step 6: Run tests to verify failure**

```
go test ./internal/cli/ -run TestDoctor_Orphan -v
```

Expected: FAIL.

- [ ] **Step 7: Implement scan + flag in cmd_doctor.go**

The scanner needs candidate paths. Walk the repo (skip `.git`).

In `internal/cli/cmd_doctor.go`:

```go
import (
	// ... existing
	"io/fs"
	"path/filepath"
)
```

Add two flags:

```go
orphanMode    := fs.Bool("orphan-mode", false, "scan for orphan-mode files and report them")
restoreOrphan := fs.Bool("restore-orphan-mode", false, "with --repair, also restore writable mode on orphan-mode files (implies --orphan-mode)")
```

Gate the walk — default `loto doctor` (no flags) must NOT walk the tree. After the existing audit/output block but before the `--dry-run` and `--repair` returns, add:

```go
var orphans []string
if *orphanMode || *restoreOrphan {
	candidates := walkRepoCandidates(repoTop)
	orphans, _ = rt.Store.ScanOrphanModes(rt.Ctx, candidates)
	for _, p := range orphans {
		rel, err := filepath.Rel(repoTop, p)
		if err != nil { rel = p }
		fmt.Fprintf(stdout, "⚠ orphan-mode target=%s\n", rel)
	}
}
```

And in the `--repair` branch, after `DoctorRepair`:

```go
if *restoreOrphan && len(orphans) > 0 {
	restored := rt.Store.RestoreOrphanMode(orphans)
	fmt.Fprintf(stdout, "✓ restored-orphan-mode count=%d\n", len(restored))
}
```

Add helper at the bottom of `cmd_doctor.go`. Skip directories that don't carry source we'd ever lock:

```go
var walkSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true,
	"dist": true, "build": true, "target": true, ".cache": true,
}

func walkRepoCandidates(root string) []string {
	if root == "" { return nil }
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil { return nil }
		if d.IsDir() {
			if walkSkipDirs[d.Name()] { return filepath.SkipDir }
			return nil
		}
		if !d.Type().IsRegular() { return nil }
		out = append(out, p)
		return nil
	})
	return out
}
```

- [ ] **Step 8: Run tests**

```
go test ./internal/cli/ -run TestDoctor -v
```

Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/store/doctor.go internal/store/doctor_test.go internal/cli/cmd_doctor.go internal/cli/cmd_doctor_test.go
git commit -m "doctor: orphan-mode scan + --restore-orphan-mode flag"
```

---

## Task 10: Render package for multi-target output

**Files:**
- Create: `internal/render/cli.go`
- Test: `internal/render/cli_test.go`

Per `design.md`: triage count first, deterministic sort, key=value rows, no pluralized prose. The store now returns structured data (`MultiConflictError`, `[]ReleaseResult`, `*ChmodFailureError`); render turns it into bytes.

- [ ] **Step 1: Write failing test**

Create `internal/render/cli_test.go`:

```go
package render

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

func TestEmitLockSuccess_SortedDeterministic(t *testing.T) {
	var buf bytes.Buffer
	EmitLockSuccess(&buf, []domain.Target{
		{Canonical: "z.go", Kind: domain.KindFile},
		{Canonical: "a.go", Kind: domain.KindFile},
	})
	got := buf.String()
	wantHead := "✓ locked count=2\n"
	if !strings.HasPrefix(got, wantHead) {
		t.Errorf("first line want %q, got: %s", wantHead, got)
	}
	if strings.Index(got, "target=a.go") > strings.Index(got, "target=z.go") {
		t.Errorf("not sorted: %s", got)
	}
}

func TestEmitConflict_TriageFirst(t *testing.T) {
	now := time.Date(2026, 5, 10, 18, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	EmitConflict(&buf, &store.MultiConflictError{
		Blockers: []domain.LockRecord{
			{Target: domain.Target{Canonical: "a.go"}, OwnerUUID: "Green", Intent: "x", ExpiresAt: now},
			{Target: domain.Target{Canonical: "c.go"}, OwnerUUID: "Red",   Intent: "y", ExpiresAt: now},
		},
	})
	got := buf.String()
	if !strings.HasPrefix(got, "✗ blocked count=2\n") {
		t.Errorf("triage first: %s", got)
	}
}

func TestEmitReleaseResults_MixedOutcomes(t *testing.T) {
	var buf bytes.Buffer
	exit := EmitReleaseResults(&buf, []store.ReleaseResult{
		{Target: domain.Target{Canonical: "a.go"}, State: store.StateUnlocked},
		{Target: domain.Target{Canonical: "b.go"}, State: store.StateNoLock},
		{Target: domain.Target{Canonical: "c.go"}, State: store.StateNotOwner, Holder: "BlueOak"},
	})
	if exit != 1 { t.Errorf("any not-owner → exit 1, got %d", exit) }
	got := buf.String()
	if !strings.Contains(got, "✓ unlocked count=1\n") {
		t.Errorf("triage count = successful releases only: %s", got)
	}
	if !strings.Contains(got, "state=no-lock") || !strings.Contains(got, "state=not-owner") {
		t.Errorf("missing distinct states: %s", got)
	}
	if !strings.Contains(got, "holder=BlueOak") {
		t.Errorf("missing holder: %s", got)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/render/ -v
```

Expected: FAIL — package doesn't exist.

- [ ] **Step 3: Implement**

Create `internal/render/cli.go`:

```go
// Package render formats CLI output per docs/design.md:
// triage count on the first body line, deterministic sort, key=value rows,
// no pluralized prose, no ANSI. All target paths printed cwd-relative when
// possible — absolute paths from the store are converted at the surface.
package render

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
	"loto/internal/store"
)

// relPath returns p relative to cwd if that's a clean descent; else p unchanged.
// Uses filepath.IsLocal (Go 1.20+) to test "doesn't escape cwd" — this avoids
// the strings.HasPrefix(rel, "..") false-positive on paths like "..foo/bar"
// (a legitimate descent into a dir whose name starts with two dots).
func relPath(p string) string {
	cwd, err := os.Getwd()
	if err != nil { return p }
	rel, err := filepath.Rel(cwd, p)
	if err != nil || !filepath.IsLocal(rel) { return p }
	return rel
}

func EmitLockSuccess(w io.Writer, targets []domain.Target) {
	sorted := append([]domain.Target(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Canonical < sorted[j].Canonical })
	fmt.Fprintf(w, "✓ locked count=%d\n", len(sorted))
	for _, t := range sorted {
		fmt.Fprintf(w, "✓ target=%s\n", relPath(t.Canonical))
	}
}

func EmitConflict(w io.Writer, ce *store.MultiConflictError) {
	blockers := append([]domain.LockRecord(nil), ce.Blockers...)
	sort.Slice(blockers, func(i, j int) bool {
		return blockers[i].Target.Canonical < blockers[j].Target.Canonical
	})
	fmt.Fprintf(w, "✗ blocked count=%d\n", len(blockers))
	for i := range blockers {
		b := &blockers[i]
		fmt.Fprintf(w, "⚠ target=%s blocker=%s intent=%q expires_at=%s\n",
			relPath(b.Target.Canonical), b.OwnerUUID, b.Intent,
			b.ExpiresAt.UTC().Format(time.RFC3339))
	}
}

func EmitChmodFailure(w io.Writer, cfe *store.ChmodFailureError) {
	failed := 0
	for _, f := range cfe.Failures {
		if !f.RolledBack && f.Err != nil { failed++ }
	}
	fmt.Fprintf(w, "✗ chmod-failed count=%d\n", failed)
	sorted := append([]store.ChmodFailure(nil), cfe.Failures...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	for _, f := range sorted {
		path := relPath(f.Target.Canonical)
		switch {
		case f.Err != nil && !f.RolledBack:
			fmt.Fprintf(w, "✗ target=%s err=%v rolled-back=no\n", path, f.Err)
		case f.Err != nil && f.RolledBack:
			fmt.Fprintf(w, "✗ target=%s err=%v rolled-back=yes\n", path, f.Err)
		default:
			fmt.Fprintf(w, "✓ target=%s state=restored\n", path)
		}
	}
}

func EmitInvalid(w io.Writer, items []InvalidTarget) {
	sort.Slice(items, func(i, j int) bool { return items[i].Path < items[j].Path })
	fmt.Fprintf(w, "✗ invalid count=%d\n", len(items))
	for _, it := range items {
		fmt.Fprintf(w, "✗ target=%s reason=%s\n", relPath(it.Path), it.Reason)
	}
}

type InvalidTarget struct {
	Path   string
	Reason string // e.g. "not-regular-file", "not-found", "symlink", "duplicate-target", "stat-failed: ..."
}

// EmitReleaseResults renders per-target outcomes and returns the suggested
// exit code: 0 if no not-owner / restore-failed rows, 1 otherwise.
// Renders canonical-sorted regardless of input order (caller passes input order;
// render owns deterministic output).
func EmitReleaseResults(w io.Writer, results []store.ReleaseResult) int {
	sorted := append([]store.ReleaseResult(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Target.Canonical < sorted[j].Target.Canonical })
	successCount := 0
	exit := 0
	for _, r := range sorted {
		if r.State == store.StateUnlocked { successCount++ }
		if r.State == store.StateNotOwner || r.State == store.StateRestoreFailed { exit = 1 }
	}
	fmt.Fprintf(w, "✓ unlocked count=%d\n", successCount)
	for _, r := range sorted {
		path := relPath(r.Target.Canonical)
		switch r.State {
		case store.StateUnlocked:
			fmt.Fprintf(w, "✓ target=%s\n", path)
		case store.StateNoLock:
			fmt.Fprintf(w, "ℹ target=%s state=no-lock\n", path)
		case store.StateNotOwner:
			fmt.Fprintf(w, "✗ target=%s state=not-owner holder=%s\n", path, r.Holder)
		case store.StateRestoreFailed:
			fmt.Fprintf(w, "⚠ target=%s state=restore-failed err=%v\n", path, r.RestoreErr)
		}
	}
	return exit
}
```

- [ ] **Step 4: Run tests**

```
go test ./internal/render/ -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/render/
git commit -m "render: multi-target output (triage count first, deterministic, key=value)"
```

---

## Task 11: cmd_lock multi-target

**Files:**
- Modify: `internal/cli/cmd_lock.go`
- Test: `internal/cli/cmd_lock_test.go`

Replace `fs.NArg() != 1` with N-arg validation. Reject directories, non-existent paths, non-regular files. Use `render.EmitLockSuccess` / `EmitConflict` / `EmitChmodFailure` / `EmitInvalid`.

- [ ] **Step 1: Write failing tests**

Replace existing `TestLockHappyPath` / add new tests in `internal/cli/cmd_lock_test.go`:

```go
func TestLock_MultiTarget_HappyPath(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	for _, n := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	var out bytes.Buffer
	code := Run([]string{tcCmdLock, "a.go", "b.go", tcFlagIntent, "test"}, &out, io.Discard)
	if code != 0 { t.Fatalf("exit %d: %s", code, out.String()) }
	if !strings.HasPrefix(out.String(), "✓ locked count=2\n") {
		t.Errorf("missing triage line: %s", out.String())
	}
	for _, n := range []string{"a.go", "b.go"} {
		st, _ := os.Stat(filepath.Join(repo, n))
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s not stripped: %o", n, st.Mode().Perm())
		}
	}
}

func TestLock_RejectDirectoryTarget(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.MkdirAll(filepath.Join(repo, "internal/store"), 0o755); err != nil { t.Fatal(err) }
	var errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "internal/store/"}, io.Discard, &errBuf)
	if code != 2 { t.Fatalf("exit %d, want 2: %s", code, errBuf.String()) }
	if !strings.Contains(errBuf.String(), "not a regular file") {
		t.Errorf("missing reject text: %s", errBuf.String())
	}
}

func TestLock_RejectNonExistentTarget(t *testing.T) {
	withTempProject(t); pinAgent(t)
	var errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "missing.go"}, io.Discard, &errBuf)
	if code != 2 { t.Fatalf("exit %d, want 2: %s", code, errBuf.String()) }
	if !strings.Contains(errBuf.String(), "not-found") {
		t.Errorf("expected reason=not-found: %s", errBuf.String())
	}
}

func TestLock_RejectsDuplicateTargets(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	if err := os.WriteFile(filepath.Join(repo, "a.go"), []byte("x"), 0o644); err != nil { t.Fatal(err) }
	var errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "a.go", "a.go"}, io.Discard, &errBuf)
	if code != 2 { t.Fatalf("exit %d, want 2: %s", code, errBuf.String()) }
	if !strings.Contains(errBuf.String(), "duplicate-target") {
		t.Errorf("expected duplicate-target: %s", errBuf.String())
	}
}

func TestLock_RejectsSymlinks(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	target := filepath.Join(repo, "real.go")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	link := filepath.Join(repo, "link.go")
	if err := os.Symlink(target, link); err != nil { t.Fatal(err) }
	var errBuf bytes.Buffer
	code := Run([]string{tcCmdLock, "link.go"}, io.Discard, &errBuf)
	if code != 2 { t.Fatalf("exit %d, want 2: %s", code, errBuf.String()) }
	if !strings.Contains(errBuf.String(), "symlink") {
		t.Errorf("expected reason=symlink: %s", errBuf.String())
	}
}

func TestLock_MultiFileFlagsApplyToAllTargets(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	for _, n := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}
	if code := Run([]string{tcCmdLock, "a.go", "b.go", tcFlagIntent, "shared", "--ttl", "10m"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("lock failed")
	}
	// Inspect via status JSON or store directly. Cheapest: re-acquire as same agent
	// and confirm both rows have intent=shared by issuing `loto status --mine`.
	var out bytes.Buffer
	if code := Run([]string{tcCmdStatus, tcFlagMine}, &out, io.Discard); code != 0 {
		t.Fatal("status failed")
	}
	count := strings.Count(out.String(), "intent=\"shared\"")
	if count != 2 {
		t.Errorf("expected intent on both rows, got %d in: %s", count, out.String())
	}
}
```

(Adjust `tcFlagMine`/`intent=` formatting based on what `cmd_status` actually prints — check `internal/cli/cmd_status.go` and adjust if different.)

- [ ] **Step 2: Run failing tests**

```
go test ./internal/cli/ -run TestLock_ -v
```

Expected: FAIL.

- [ ] **Step 3: Rewrite cmd_lock.go**

Replace `internal/cli/cmd_lock.go`:

```go
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"time"

	"loto/internal/domain"
	"loto/internal/render"
	"loto/internal/store"
)

func init() { register("lock", cmdLock) } //nolint:gochecknoinits // command registry pattern

func cmdLock(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("lock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ttl := fs.Duration("ttl", 30*time.Minute, "lock TTL")
	intent := fs.String("intent", "", "free-text intent")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: loto lock <file>... [--ttl 30m] [--intent ...]")
		return 2
	}

	// Validate ALL targets before opening runtime — zero side effects on rejection.
	targets := make([]domain.Target, 0, fs.NArg())
	seen := make(map[string]bool, fs.NArg())
	var invalid []render.InvalidTarget
	for _, raw := range fs.Args() {
		t, err := domain.Canonicalize(raw)
		if err != nil {
			invalid = append(invalid, render.InvalidTarget{Path: raw, Reason: err.Error()})
			continue
		}
		if seen[t.Canonical] {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "duplicate-target"})
			continue
		}
		seen[t.Canonical] = true
		// Lstat so symlinks are visible (a hand-tool resolves nothing implicitly).
		lst, err := os.Lstat(t.Canonical)
		if err != nil {
			reason := fmt.Sprintf("stat-failed: %v", err)
			if errors.Is(err, fs.ErrNotExist) { reason = "not-found" }
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: reason})
			continue
		}
		if lst.Mode()&os.ModeSymlink != 0 {
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "symlink"})
			continue
		}
		if !lst.Mode().IsRegular() {
			fmt.Fprintf(stderr, "%s: not a regular file. loto locks files; for directories, pass the file list (e.g. loto lock $(fd . internal/store -e go)).\n", t.Canonical)
			invalid = append(invalid, render.InvalidTarget{Path: t.Canonical, Reason: "not-regular-file"})
			continue
		}
		targets = append(targets, t)
	}
	if len(invalid) > 0 {
		render.EmitInvalid(stderr, invalid)
		return 2
	}

	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	live := func(host string, pid int) bool {
		if host != rt.Host { return true }
		return pidLive(pid)
	}
	now := time.Now()
	recs := make([]domain.LockRecord, len(targets))
	for i, t := range targets {
		recs[i] = domain.LockRecord{
			Target: t, OwnerUUID: rt.Agent.UUID, SessionUUID: rt.Agent.UUID,
			Intent: *intent, CreatedAt: now, ExpiresAt: now.Add(*ttl),
			Host: rt.Host, PID: os.Getpid(),
		}
	}

	_, err = rt.Store.AcquireLocks(rt.Ctx, recs, live)
	if err != nil {
		var mce *store.MultiConflictError
		var cfe *store.ChmodFailureError
		switch {
		case errors.As(err, &mce):
			render.EmitConflict(stdout, mce)
			return 1
		case errors.As(err, &cfe):
			render.EmitChmodFailure(stdout, cfe)
			return 3
		default:
			fmt.Fprintf(stderr, "✗ %v\n", err)
			return 3
		}
	}
	render.EmitLockSuccess(stdout, targets)
	return 0
}
```

- [ ] **Step 4: Run tests**

```
go test ./internal/cli/ -run TestLock_ -v
```

Expected: PASS.

- [ ] **Step 5: Run the existing TestLockConflictBetweenAgents**

```
go test ./internal/cli/ -run TestLockConflictBetweenAgents -v
```

If it fails because the output format changed (`✗ blocked target=` → `✗ blocked count=`), update the assertion to check for `"✗ blocked count="` instead. Same for the two-agent acceptance test.

- [ ] **Step 6: Combined commit (store + CLI together)**

This commit folds Task 5's deferred store work with the CLI rewrite — between them `go build` would fail, so they ship as one commit for `git bisect` cleanliness.

```bash
git add internal/cli/cmd_lock.go internal/cli/cmd_lock_test.go
git commit -m "lockout: multi-target AcquireLocks + cmd_lock rewrite"
```

---

## Task 12: cmd_unlock multi-target best-effort

**Files:**
- Modify: `internal/cli/cmd_unlock.go`
- Modify: `internal/cli/cmd_lock_test.go` (where unlock tests live) or add `cmd_unlock_test.go`

- [ ] **Step 1: Write failing tests**

Add to `internal/cli/cmd_unlock_test.go` (create if needed):

```go
package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUnlock_MultiTarget_BestEffortMissingVsNotOwner(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	for _, n := range []string{"a.go", "b.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, "a.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("alice lock a") }

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	if code := Run([]string{tcCmdLock, "c.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("bob lock c") }

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	var out bytes.Buffer
	code := Run([]string{tcCmdUnlock, "a.go", "b.go", "c.go"}, &out, io.Discard)
	if code != 1 { t.Fatalf("exit %d, want 1; out=%s", code, out.String()) }

	got := out.String()
	if !strings.Contains(got, "✓ unlocked count=1\n") { t.Errorf("triage: %s", got) }
	if !strings.Contains(got, "state=no-lock") { t.Errorf("missing no-lock: %s", got) }
	if !strings.Contains(got, "state=not-owner") { t.Errorf("missing not-owner: %s", got) }
}
```

- [ ] **Step 2: Run to verify failure**

```
go test ./internal/cli/ -run TestUnlock_MultiTarget -v
```

Expected: FAIL.

- [ ] **Step 3: Rewrite cmd_unlock.go**

```go
package cli

import (
	"flag"
	"fmt"
	"io"

	"loto/internal/domain"
	"loto/internal/render"
)

func init() { register("unlock", cmdUnlock) } //nolint:gochecknoinits // command registry pattern

func cmdUnlock(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("unlock", flag.ContinueOnError)
	fs.SetOutput(stderr)
	allMine := fs.Bool("all-mine", false, "release every lock owned by my uuid")
	if err := fs.Parse(permuteWith(fs, args)); err != nil {
		return 2
	}
	rt, err := openRuntime()
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	defer rt.Close()

	if *allMine {
		return unlockAllMine(rt, stdout, stderr)
	}
	if fs.NArg() < 1 {
		fmt.Fprintln(stderr, "usage: loto unlock <file>... | --all-mine")
		return 2
	}

	targets := make([]domain.Target, 0, fs.NArg())
	for _, raw := range fs.Args() {
		t, err := domain.Canonicalize(raw)
		if err != nil {
			fmt.Fprintf(stderr, "✗ target %s: %v\n", raw, err)
			return 2
		}
		targets = append(targets, t)
	}

	results, err := rt.Store.ReleaseLocks(rt.Ctx, targets, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stderr, "✗ %v\n", err)
		return 3
	}
	return render.EmitReleaseResults(stdout, results)
}

func unlockAllMine(rt *runtime, stdout, _ io.Writer) int {
	all, err := rt.Store.ListLocks(rt.Ctx)
	if err != nil {
		fmt.Fprintf(stdout, "✗ %v\n", err)
		return 3
	}
	var targets []domain.Target
	for i := range all {
		if all[i].OwnerUUID == rt.Agent.UUID {
			targets = append(targets, all[i].Target)
		}
	}
	if len(targets) == 0 {
		fmt.Fprintln(stdout, "✓ unlocked count=0")
		return 0
	}
	results, err := rt.Store.ReleaseLocks(rt.Ctx, targets, rt.Agent.UUID)
	if err != nil {
		fmt.Fprintf(stdout, "✗ %v\n", err)
		return 3
	}
	return render.EmitReleaseResults(stdout, results)
}
```

- [ ] **Step 4: Run tests**

```
go test ./internal/cli/ -run TestUnlock -v
```

Expected: PASS. Existing `TestUnlockOwner` may need its `"✓ unlocked"` assertion adjusted to match new format `"✓ unlocked count=1"`.

- [ ] **Step 5: Combined commit (store + CLI together)**

Folds Task 6's deferred store work with this CLI rewrite, same bisect rationale.

```bash
git add internal/cli/cmd_unlock.go internal/cli/cmd_unlock_test.go
git commit -m "lockout: multi-target ReleaseLocks + cmd_unlock rewrite"
```

---

## Task 13: cmd_break confirms chmod restore at CLI level

**Files:**
- Test: `internal/cli/cmd_break_test.go`

The store layer already restores write mode (Task 8). This task verifies the CLI surface end-to-end and tightens the test.

- [ ] **Step 1: Add test**

Add to `internal/cli/cmd_break_test.go`:

```go
func TestBreak_Force_RestoresWriteMode(t *testing.T) {
	repo := withTempProject(t)
	alice, bob := twoAgents(t)
	p := filepath.Join(repo, "a.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }

	t.Setenv("LOTO_AGENT_ID", alice.UUID)
	if code := Run([]string{tcCmdLock, "a.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("alice lock") }

	t.Setenv("LOTO_AGENT_ID", bob.UUID)
	var out bytes.Buffer
	if code := Run([]string{"break", "a.go", "--force", "--reason", "stuck"}, &out, io.Discard); code != 0 {
		t.Fatalf("break exit: %s", out.String())
	}
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected restored after break, got %o", st.Mode().Perm())
	}
}
```

- [ ] **Step 2: Run**

```
go test ./internal/cli/ -run TestBreak_Force -v
```

Expected: PASS (store-level restore from Task 8 fires through the CLI unchanged).

- [ ] **Step 3: Commit**

```bash
git add internal/cli/cmd_break_test.go
git commit -m "cli: test that break --force restores write mode"
```

---

## Task 14: Concurrency test — op-flock serializes overlapping invocations

**Files:**
- Test: `internal/cli/acceptance_test.go`

- [ ] **Step 1: Write test**

Add to `internal/cli/acceptance_test.go`:

```go
func TestConcurrentLock_SerializedByOpFlock(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	for _, n := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(repo, n), []byte("x"), 0o644); err != nil { t.Fatal(err) }
	}

	var wg sync.WaitGroup
	exits := make([]int, 2)
	for i, args := range [][]string{
		{tcCmdLock, "a.go"},
		{tcCmdLock, "b.go"},
	} {
		i, args := i, args
		wg.Add(1)
		go func() {
			defer wg.Done()
			exits[i] = Run(args, io.Discard, io.Discard)
		}()
	}
	wg.Wait()
	if exits[0] != 0 || exits[1] != 0 {
		t.Errorf("exits: %v", exits)
	}
	// Both files should be stripped.
	for _, n := range []string{"a.go", "b.go"} {
		st, _ := os.Stat(filepath.Join(repo, n))
		if st.Mode().Perm()&0o222 != 0 {
			t.Errorf("%s not stripped: %o", n, st.Mode().Perm())
		}
	}
}
```

Add `sync`, `os`, `path/filepath`, `io` imports as needed.

- [ ] **Step 2: Run**

```
go test ./internal/cli/ -run TestConcurrentLock -v -race
```

Expected: PASS (and clean under `-race`).

- [ ] **Step 3: Commit**

```bash
git add internal/cli/acceptance_test.go
git commit -m "cli: test op-flock serializes concurrent lock invocations"
```

---

## Task 15: Acceptance — locked file resists third-party writes; readable

**Files:**
- Test: `internal/cli/acceptance_test.go`

- [ ] **Step 1: Write tests**

Append to `internal/cli/acceptance_test.go`:

```go
func TestLockedFile_WriteByThirdPartyReturnsEACCES(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses chmod write bits; EACCES is unreachable")
	}
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "x.go")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdLock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("lock") }

	err := os.WriteFile(p, []byte("clobber"), 0o644)
	if err == nil { t.Fatal("expected EACCES, got nil") }
	if !errors.Is(err, fs.ErrPermission) {
		t.Errorf("expected fs.ErrPermission, got %v", err)
	}
}

func TestLockedFile_StillReadable(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "x.go")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdLock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("lock") }

	got, err := os.ReadFile(p)
	if err != nil { t.Fatalf("read locked file: %v", err) }
	if string(got) != "orig" { t.Errorf("got %q", got) }
}

func TestUnlock_RestoresOwnerWrite(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "x.go")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdLock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("lock") }
	if code := Run([]string{tcCmdUnlock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("unlock") }

	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 {
		t.Errorf("expected owner-write restored, got %o", st.Mode().Perm())
	}
	if err := os.WriteFile(p, []byte("ok"), 0o644); err != nil {
		t.Errorf("write after unlock failed: %v", err)
	}
}

func TestUnlock_FileDeletedWhileHeldIsNoErrorOnRestore(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "x.go")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdLock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("lock") }
	if err := os.Remove(p); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdUnlock, "x.go"}, io.Discard, io.Discard); code != 0 {
		t.Fatal("unlock should succeed even when file was deleted while held")
	}
}

func TestDoctor_CrashRecoveryRoundTrip(t *testing.T) {
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "orphan.go")
	if err := os.WriteFile(p, []byte("x"), 0o444); err != nil { t.Fatal(err) }
	// Step 1: --orphan-mode reports but does not repair.
	var out bytes.Buffer
	if code := Run([]string{"doctor", "--orphan-mode"}, &out, io.Discard); code != 0 { t.Fatal(out.String()) }
	if !strings.Contains(out.String(), "orphan-mode") { t.Errorf("missing orphan-mode: %s", out.String()) }
	st, _ := os.Stat(p)
	if st.Mode().Perm()&0o200 != 0 { t.Errorf("flag-only should not restore: %o", st.Mode().Perm()) }
	// Step 2: --restore-orphan-mode + --repair actually restores.
	if code := Run([]string{"doctor", "--repair", "--restore-orphan-mode"}, io.Discard, io.Discard); code != 0 { t.Fatal("repair") }
	st, _ = os.Stat(p)
	if st.Mode().Perm()&0o200 == 0 { t.Errorf("expected restored: %o", st.Mode().Perm()) }
}
```

Add `errors`, `io/fs` imports if missing.

- [ ] **Step 2: Run**

```
go test ./internal/cli/ -run TestLockedFile -v
go test ./internal/cli/ -run TestUnlock_RestoresOwnerWrite -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/acceptance_test.go
git commit -m "cli: acceptance tests for lock enforcement (EACCES, readable, restore)"
```

---

## Task 16: Acceptance — chmod+w bypass documented; reject directory; reject missing

**Files:**
- Test: `internal/cli/acceptance_test.go`

- [ ] **Step 1: Write tests**

Append:

```go
func TestLockedFile_ChmodPlusWAllowsWrite(t *testing.T) {
	// Documents the known threat-model bypass: cooperating Claudes are
	// defeated, hostile users with chmod +w are not.
	repo := withTempProject(t)
	pinAgent(t)
	p := filepath.Join(repo, "x.go")
	if err := os.WriteFile(p, []byte("orig"), 0o644); err != nil { t.Fatal(err) }
	if code := Run([]string{tcCmdLock, "x.go"}, io.Discard, io.Discard); code != 0 { t.Fatal("lock") }

	if err := os.Chmod(p, 0o644); err != nil { t.Fatal(err) }
	if err := os.WriteFile(p, []byte("clobber"), 0o644); err != nil {
		t.Errorf("expected write to succeed after chmod +w, got: %v", err)
	}
}

// (TestLock_RejectDirectoryTarget and TestLock_RejectNonExistentTarget
//  already exist from Task 11.)
```

- [ ] **Step 2: Run**

```
go test ./internal/cli/ -run TestLockedFile_ChmodPlusW -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/cli/acceptance_test.go
git commit -m "cli: document chmod+w bypass as expected (threat model)"
```

---

## Task 17: Full sweep — green build + race + audit

**Files:** none (verification only)

- [ ] **Step 1: Run full test suite with race**

```
go test ./... -race
```

Expected: PASS across all packages.

- [ ] **Step 2: Run audit (most stringent)**

```
make audit
```

If `make audit` is unavailable, fall back to:

```
make check
```

Expected: green. Fix any new lint findings inline (godot, goconst, errcheck) — the simplest patterns from `go-lintbrush:polish`.

- [ ] **Step 3: Commit any cleanup if needed**

```bash
git add -p
git commit -m "lockout: lint cleanup from full audit pass"
```

If the audit was clean, skip the commit.

---

## Task 18: Update boot.md, close gh#57, close beads

**Files:**
- Modify: `.claude/rules/boot.md`

- [ ] **Step 1: Update boot.md**

Edit `.claude/rules/boot.md`:

- Remove the line `→ ship lockout primitive (gh#57)...`
- Remove the trap `Phase 5 hooks blocked on gh#57 — hook alone is post-it, not enforcement` (or rephrase: "now ready: Phase 5 hook gate (Tasks 21-22) — `loto hook pre-write` plus install-hook --write-gate")
- Add to ✓ done: `lockout primitive shipped (gh#57): chmod strip-write enforcement, multi-target atomic acquire, op-flock serialization, doctor orphan-mode scan`

- [ ] **Step 2: Close gh#57 with summary**

```bash
gh issue close 57 --comment "$(cat <<'EOF'
Lockout primitive shipped. Multi-target atomic AcquireLock with chmod strip-write, project op-flock serialization, per-target best-effort ReleaseLock with distinct missing/not-owner outputs (folds gh#46), BreakLock + lazy GC chmod-restore, doctor orphan-mode scan with --restore-orphan-mode flag.

Plan: docs/superpowers/plans/2026-05-10-lockout-primitive.md
Spec: docs/superpowers/specs/2026-05-10-lockout-primitive-design.md
EOF
)"
```

- [ ] **Step 3: Also close gh#46**

```bash
gh issue close 46 --comment "Folded into gh#57 lockout work: ReleaseLocks now distinguishes StateNoLock (ℹ no-lock) from StateNotOwner (✗ not-owner holder=...) at both the store and CLI surfaces. See cmd_unlock.go + render.EmitReleaseResults."
```

- [ ] **Step 4: File the NORTH_STAR doc-debt bead**

The plan promoted multi-file atomic acquire — currently listed as a non-goal in `docs/NORTH_STAR.md` — to first-class. Queue a one-line doc edit so docs and code don't desync. File alongside the existing `loto-9ky` / `loto-qy6` doc-debt beads:

```bash
bd q "NORTH_STAR.md: remove 'multi-file atomic acquire (yet — YAGNI)' from non-goals; cite cooperating-Claudes-mid-sweep use case"
```

- [ ] **Step 5: Final commit**

```bash
git add .claude/rules/boot.md
git commit -m "boot: lockout primitive shipped — close gh#57, gh#46"
```

---

## Self-Review

**Spec coverage:** Walked through each spec section.
- ✓ chmod policy (no stored mode) — Task 2 + Task 5 (`stripWrite`/`restoreWrite` use bitmask only).
- ✓ scope contract (files only, multi-file atomic, reject dir/missing) — Task 11.
- ✓ lock operation (op-flock, validate-before-tx, blockers, chmod, rollback, system tag on rollback failure) — Task 5.
- ✓ unlock operation (per-target, missing vs not-owner, exit 1 if any not-owner) — Task 6 + Task 12.
- ✓ break --force restores mode — Task 8 (store) + Task 13 (CLI test).
- ✓ output shapes (conflict, chmod-failed, invalid, mixed unlock) — Task 10.
- ✓ crash recovery (lazy GC chmod-restore, orphan-mode flag-only by default) — Task 7, Task 9.
- ✓ migration (bump user_version, MoveCorruptAside on mismatch) — Task 1.
- ✓ schema change: none — Task 1 only bumps user_version.
- ✓ file boundaries — matches the file map at top of plan.
- ✓ acceptance tests — covered across Tasks 11-16.
- ✓ existing tests ported (TestLockHappyPath replaced; assertions for new output format updated in Task 11 step 5 and Task 12 step 4).
- ✓ smell-test acknowledgements — op-flock blocking is documented in the helper godoc; mode lossiness is a chmod-policy fact.
- ✓ side-effect asymmetry in `reclaimStaleTx` — documented inline in Task 7 Step 3.

**Placeholder scan:** No "TBD", "implement later", or "similar to Task N" — every step has actual code or commands. No floating revisions overlay; round-2 and round-3 review patches are folded into the task bodies (Task 7 narrative scrubbed; final spy-on-chmodFn test presented directly).

**Round-3 review integration (this revision):**
- Hardlinked files (`Nlink > 1`) are rejected — Task 5 store-level Lstat block. Closes the inode-leak hole (A locks `foo.go`, B locks `foo-alias.go`, A unlocks restores shared inode while B still holds).
- Task 1 schema check now uses `os.Stat`-before-`sql.Open` to distinguish brand-new DB from pre-versioned DB; the bogus `v != 0 && v != schemaUserVersion` exception is gone.
- `mode_restore_failed` tag is written *after* tx rollback in a fresh statement, not while the rolling tx is still open. Eliminates SQLite footgun.
- Store-level Lstat+IsRegular+symlink+Nlink validation added inside `AcquireLocks` under the op-flock — the store defends itself, CLI validation is convenience.
- `BreakLock` now takes the project op-flock — closes the serialization gap between break and acquire/release.
- `flockStderr` global removed; stderr writer threaded as a parameter into `acquireOpFlock` and stored on `Store`. Fixes the `-race` data hazard.
- `relPath` uses `filepath.IsLocal` instead of `strings.HasPrefix(rel, "..")`. Fixes the `..foo/bar` false-positive.
- Added `TestOpFlock_TimeoutAborts` and `TestAcquireLocks_RollbackRestoreFailureLeavesBreadcrumb`.

**Round-3 review items consciously NOT integrated:**
- Bisect-defer task folding (Claude P2): combined-commit ceremony in Tasks 11/12 already explicitly documents the rationale; restructuring risk > benefit.
- Non-Unix build stub (ChatGPT #9): personal-tool on macOS; `//go:build unix` on flock.go already exists; a hard-fail on non-Unix is acceptable.
- `LOTO_FLOCK_NOTICE_AFTER` env var (Claude P3): YAGNI for now.
- Doctor "orphan-mode-candidate" wording (ChatGPT #11): kept short form; the `--restore-orphan-mode` gate already documents user intent.

**Type consistency:** `AcquireLocks` / `ReleaseLocks` / `MultiConflictError` / `ChmodFailureError` / `ChmodFailure` / `ReleaseResult` / `ReleaseOutcome` / `StateUnlocked|StateNoLock|StateNotOwner` / `InvalidTarget` — names used identically across Tasks 5, 6, 10, 11, 12.

**Gaps not covered (deferred per spec non-goals):**
- Hook gate (Tasks 21-22 of original v2 plan) — out of scope; spec lists this as deferred-but-unblocked.
- `loto-9ky` and `loto-qy6` doc-debt beads — separate one-line edits, tracked outside this plan.
- JSON output (`--json` flag) — spec says "confirm exact shape during implementation in `internal/render/cli.go`"; not a hard requirement for this PR. Add as follow-up if needed.

---

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-05-10-lockout-primitive.md`. Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch with checkpoints.

Which approach?
