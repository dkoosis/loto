# Plan — loto-qqh.1: Foundations (schema bump + chmod helpers + op-flock + path resolution)

**Source:** snapshot of plan tasks 1–4 from `docs/superpowers/plans/2026-05-10-lockout-primitive.md` (loaded verbatim per dispatch-bead-agent pre-written-plan short-circuit).

**Bead:** loto-qqh.1 — closes part of gh#57 (parent epic loto-qqh).

**Scope:** Tasks 1–4 only. Tasks 5–18 belong to sibling beads (loto-qqh.2 … loto-qqh.8). Do not touch AcquireLock/ReleaseLock/BreakLock/render/CLI/doctor here.

**Files (in scope for this bead):**
- New: `internal/store/chmod.go` (+ `internal/store/chmod_test.go`)
- New: `internal/store/flock.go` (with `//go:build unix`) (+ test)
- Modify: `internal/store/schema.sql`
- Modify: `internal/store/store.go` (Open path — schema-version check + wipe-on-mismatch; project-root path resolution helper)
- Modify: `internal/store/store_test.go`

**Out of scope (sibling beads):**
- `AcquireLock` / `ReleaseLock` / `BreakLock` body changes — loto-qqh.2 / .3
- `collectBlockers` lazy GC — loto-qqh.3
- doctor changes — loto-qqh.4
- render package — loto-qqh.5
- CLI surfaces — loto-qqh.6

**Acceptance for this bead:**
- `make audit` green on worktree.
- New helpers exist, are package-private where appropriate, and are covered by unit tests.
- Existing AcquireLock/ReleaseLock/BreakLock behavior **unchanged at the surface** (no row-shape or chmod side-effects yet).
- Schema bump migrates fresh-open correctly; mismatch wipes and reopens.

---

## Implementer notes (from P-self craft review)

‡ Read before starting. These resolve cross-task ambiguities in the verbatim plan body below.

1. **`Store` struct end-state (after all four tasks):** `{ db *sql.DB, dbPath string, stderr io.Writer }`. Tasks 3 and 4 each show a `type Store struct` block — those are **incremental**, not resets. When applying Task 4, keep the `stderr` field added in Task 3. `openOnce` initialises both: `&Store{db: db, dbPath: p, stderr: os.Stderr}`.

2. **`schema.sql` edit (Task 1):** Current `schema.sql` has no `PRAGMA user_version` directive. Add `PRAGMA user_version = 3;` (as a top-level statement, after the header comment, before the `CREATE TABLE` blocks). The Go-side `schemaUserVersion = 3` constant must match.

3. **Task 4 is just `opFlockPath()` derivation** — no flock acquisition wiring at the store-public-API level happens in this bead. That belongs to loto-qqh.2 (AcquireLock). Title says "Store-level op-flock path resolution" — read it as "expose the path; don't take the flock yet."

4. **Per-task `make audit` checkpoints.** Run `make audit` after each Task's commit, not just at the end of the bead. Keeps the bisection window small if something regresses. Cheap.

5. **Build tag isolation.** Both `internal/store/flock.go` and `internal/store/flock_test.go` carry `//go:build unix`. Plan body shows this for both; preserve it.

6. **Pre-existing helpers in scope:** `MoveCorruptAside` (`internal/store/doctor.go:74`) and `isCorruptDB` (`internal/store/store.go:56`) already exist. Task 1's `Open` refactor calls into them — do not redefine.

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

In `internal/store/schema.sql`, after the header comment and before the first `CREATE TABLE`, insert:

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

func TestStripWrite_MissingFileReturnsError(t *testing.T) {
	// Asymmetric with restoreWrite: stripping a missing file is an error
	// (we cannot strip what isn't there). Spec-correct; pinned by this test
	// so the asymmetry stays intentional.
	err := stripWrite(filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist, got %v", err)
	}
}
```

Add `"errors"` and `"io/fs"` to the test imports.

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

	var inHold atomic.Int32
	var maxConcurrent atomic.Int32
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h, err := acquireOpFlock(path, nil)
			if err != nil { t.Errorf("acquire: %v", err); return }
			defer h.release()
			cur := inHold.Add(1)
			// Track the highest observed concurrent-holder count.
			for {
				m := maxConcurrent.Load()
				if cur <= m || maxConcurrent.CompareAndSwap(m, cur) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			inHold.Add(-1)
		}()
	}
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("op-flock did not serialize: max concurrent holders = %d, want 1", got)
	}
}
```

Add `"sync/atomic"` to the imports list above.

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

In `internal/store/store.go`, the `Store` struct already carries `db` and (from Task 3) `stderr`. **Add a `dbPath` field; do NOT remove `stderr`.** End-state struct:

```go
type Store struct {
	db     *sql.DB
	dbPath string
	stderr io.Writer
}
```

Update `openOnce` to set `s := &Store{db: db, dbPath: p, stderr: os.Stderr}`.

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

