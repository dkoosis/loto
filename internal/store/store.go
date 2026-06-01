package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schemaUserVersion = 9

var errUserVersionMismatch = errors.New("loto: schema user_version mismatch")

type Store struct {
	db     *sql.DB
	dbPath string
	stderr io.Writer
}

// connDSN: WAL + busy_timeout + immediate-mode write txns.
func connDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
}

// Open opens the loto store at path. On a real sqlite corruption error
// (SQLITE_CORRUPT or SQLITE_NOTADB, errno-checked — not string-matched),
// the existing DB and its -wal/-shm siblings are moved aside atomically
// and a fresh DB is created. Recovery is serialized via flock on a
// sidecar lock file so concurrent openers cannot interleave.
//
// First-Open is serialized on the project op-flock: two processes
// creating the same DB simultaneously would otherwise both pass the
// existence check and clobber each other's writes. Subsequent Opens on
// an initialized DB take the fast path (no flock).
func Open(p string) (*Store, error) {
	return OpenContext(context.Background(), p)
}

// OpenContext is Open with a caller-supplied context. Cancellation aborts
// flock polling (op-flock + recovery-lock) instead of waiting out
// LOTO_FLOCK_TIMEOUT.
func OpenContext(ctx context.Context, p string) (*Store, error) {
	return acquireOpenLocks(ctx, p)
}

// acquireOpenLocks is the single canonical entry point for the Open-path
// lock dance. It enforces the gh#109 invariant:
//
//	op-flock is NEVER held across acquireRecoveryLock.
//
// Op-flock protects only the create-race window on fresh DBs (two
// concurrent first-Opens picking the same path). Recovery-lock serializes
// corrupt-DB recovery and is taken alone. Holding both at once would
// (a) be one missing rename away from an AB/BA deadlock with any future
// caller that takes them in the opposite order, and (b) stall every
// unrelated `loto` invocation for the full recovery poll window, since
// acquire/release/break/doctor all need op-flock.
//
// Canonical order, when both are needed by a single caller: op-flock
// first, then release before recovery-lock. The fresh-DB path here
// follows that rule by releasing op-flock immediately after the initial
// openOnce attempt — before any recovery-lock acquire — even when the
// initial attempt fails with corruption/version mismatch.
func acquireOpenLocks(ctx context.Context, p string) (*Store, error) {
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		// A non-empty file on disk is NOT proof the DB is usable: a
		// concurrent first-Open creates the file and begins writing the
		// schema/WAL well before it stamps user_version, so a peer that
		// stats here mid-create would see size>0 and, on the old gate,
		// skip op-flock straight into openWithRecovery — racing the
		// in-flight create/migrate (loto-qev1: SQLITE_IOERR 1802,
		// SQLITE_BUSY, or a user_version=0 mismatch that then triggered a
		// bogus move-aside).
		//
		// Gate the fast lock-free path on the DB being PROVABLY initialized
		// (user_version == schemaUserVersion), probed lock-free. Any probe
		// error — including the transient I/O/BUSY of a mid-create DB — is
		// treated as "not yet initialized" and falls through to the
		// op-flock-guarded path, which serializes behind the create.
		if dbInitialized(ctx, p) {
			// Steady state: DB already stamped. No create race possible,
			// so op-flock isn't needed. openWithRecovery may take
			// recovery-lock alone.
			return openWithRecovery(ctx, p)
		}
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat db path: %w", err)
	}

	// Fresh-or-mid-create path: op-flock guards the create-race window, but
	// is released before any recovery-lock acquire (gh#109).
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	flock, err := acquireOpFlock(ctx, opFlockPathFor(p), os.Stderr)
	if err != nil {
		return nil, err
	}
	// Safety net for an unwind (panic) through the gap below: release()
	// nil-guards, so on the normal path this is a no-op once we've nulled
	// the handle's file after the explicit release. This must NOT replace
	// the explicit release at the end of the gap — the gh#109 invariant
	// requires op-flock be freed BEFORE any recovery-lock acquire, and a
	// bare defer would only fire after openWithRecovery returns.
	defer flock.release()
	s, openErr := openOnce(ctx, p)
	// Release op-flock BEFORE any recovery-lock acquire. If openOnce
	// succeeded the create race is resolved; if it failed with corruption
	// or version mismatch, openWithRecovery will retake recovery-lock
	// alone — never with op-flock held. Null the handle's file afterward so
	// the deferred safety-net release above becomes a no-op (release()
	// nil-guards on h.f == nil), preserving the explicit-before-recovery
	// ordering.
	flock.release()
	flock.f = nil
	if openErr == nil {
		return s, nil
	}
	if !isCorruptDB(openErr) && !isUserVersionMismatch(openErr) {
		return nil, openErr
	}
	return openWithRecovery(ctx, p)
}

// dbInitialized reports whether the DB at p is a fully-initialized loto store
// — i.e. a lock-free read sees user_version == schemaUserVersion. It is the
// gate for the fast lock-free Open path: only a stamped DB skips the op-flock.
//
// Crucially it is conservative on EVERY failure. A DB mid-create (concurrent
// first-Open) may transiently return SQLITE_IOERR(1802)/SQLITE_BUSY or a stale
// user_version=0; all of those return false here, routing the caller into the
// op-flock-guarded path where it serializes behind the in-flight create rather
// than racing it (loto-qev1). False negatives are cheap (one extra flock
// acquire on an already-good DB under contention); a false positive would
// reintroduce the race, so the bias is deliberate.
func dbInitialized(ctx context.Context, p string) bool {
	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return false
	}
	defer db.Close()
	// No separate PingContext: QueryRowContext establishes the connection and
	// its error covers the same transient mid-create failures (IOERR/BUSY),
	// so a dedicated ping would only double the fast-path round-trips.
	var v int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return false
	}
	return v == schemaUserVersion
}

// opFlockPathFor returns the op-flock path for a DB at p — used during
// Open() before a *Store exists.
func opFlockPathFor(p string) string {
	return filepath.Join(filepath.Dir(p), "lock-op.flock")
}

func openWithRecovery(ctx context.Context, p string) (*Store, error) {
	s, err := openOnce(ctx, p)
	if err == nil {
		return s, nil
	}
	if !isCorruptDB(err) && !isUserVersionMismatch(err) {
		return nil, err
	}

	release, lockErr := acquireRecoveryLock(ctx, p)
	if lockErr != nil {
		return nil, fmt.Errorf("acquire recovery lock: %w (orig: %w)", lockErr, err)
	}
	defer release()

	// Re-probe under the lock — another process may have already recovered.
	if s2, err2 := openOnce(ctx, p); err2 == nil {
		return s2, nil
	} else if !isCorruptDB(err2) && !isUserVersionMismatch(err2) {
		return nil, err2
	}

	moved, mvErr := moveCorruptAside(p, time.Now())
	if mvErr != nil {
		return nil, fmt.Errorf("incompatible DB and move-aside failed: %w (orig: %w)", mvErr, err)
	}
	if isUserVersionMismatch(err) {
		fmt.Fprintf(os.Stderr, "loto: incompatible DB schema moved aside to %s; created fresh DB\n", moved)
	} else {
		fmt.Fprintf(os.Stderr, "loto: corrupt DB moved aside to %s; creating fresh DB\n", moved)
	}
	return openOnce(ctx, p)
}

// openOnceHook is a test seam fired at the very top of openOnce, inside the
// op-flock gap of the fresh-DB path. Nil in production. Tests set it to inject
// a panic and assert the op-flock is still released on unwind (loto-8yst).
var openOnceHook func()

func openOnce(ctx context.Context, p string) (*Store, error) {
	if openOnceHook != nil {
		openOnceHook()
	}
	preExisted := false
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		preExisted = true
	}

	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	if preExisted {
		var v int
		if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
			db.Close()
			return nil, fmt.Errorf("read user_version: %w", err)
		}
		// A version mismatch only forces move-aside when the DB is genuinely
		// incompatible. A STALE version (below current) on a structurally-intact
		// loto schema is the loto-vmym window: a crash between the schema-tx
		// commit and the separate user_version PRAGMA write, or a DB created
		// before schemaUserVersion was bumped. Those re-migrate idempotently in
		// place (migrate re-stamps the version) rather than destroying live
		// locks. A FUTURE version (above current), or a foreign schema with no
		// `locks` table, is still moved aside.
		if v != schemaUserVersion && (v > schemaUserVersion || !schemaStructurallyIntact(ctx, db)) {
			db.Close()
			return nil, fmt.Errorf("%w: have %d, want %d", errUserVersionMismatch, v, schemaUserVersion)
		}
	}

	s := &Store{db: db, dbPath: p, stderr: os.Stderr}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// isCorruptDB returns true only for real sqlite errno results indicating
// an unreadable database file: SQLITE_CORRUPT (11) or SQLITE_NOTADB (26).
// The previous string-match implementation false-positived on any wrapped
// error containing "malformed" and destroyed healthy databases (gh#48).
// Primary code is masked off any extended-code bits per the sqlite spec.
func isCorruptDB(err error) bool {
	if err == nil {
		return false
	}
	var sqErr *sqlite.Error
	if !errors.As(err, &sqErr) {
		return false
	}
	primary := sqErr.Code() & 0xff
	return primary == sqlite3.SQLITE_CORRUPT || primary == sqlite3.SQLITE_NOTADB
}

func isUserVersionMismatch(err error) bool { return errors.Is(err, errUserVersionMismatch) }

// schemaStructurallyIntact reports whether the core loto `locks` table exists,
// the sentinel for "this is a loto DB with a stale version stamp" vs "a foreign
// or incompatibly-old DB". Used by the openOnce version gate (loto-vmym) to
// decide re-migrate-in-place over destructive move-aside. A probe failure is
// treated as not-intact (conservative: prefer move-aside on an unreadable DB).
func schemaStructurallyIntact(ctx context.Context, db *sql.DB) bool {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type='table' AND name='locks'`).Scan(&name)
	return err == nil && name == "locks"
}

// schemaFullyCurrent reports whether the DB is at the current schema in every
// respect the migrate write path would otherwise apply: the core tables exist
// AND the additive locks.proc_start column is present (loto-kwlp). It is the
// gate for migrate's steady-state no-write fast path (loto-0gsu). Unlike
// schemaStructurallyIntact — which only sentinels "is this a loto DB" for the
// move-aside decision — this must be true ONLY when a re-migrate would be a
// pure no-op, so a DB carrying the current user_version stamp but missing the
// proc_start upgrade still falls through to the full migrate. A probe failure
// is treated as not-current (conservative: prefer running migrate).
func schemaFullyCurrent(ctx context.Context, db *sql.DB) bool {
	if !schemaStructurallyIntact(ctx, db) {
		return false
	}
	for _, tbl := range []string{"events", "tags"} {
		var name string
		if err := db.QueryRowContext(ctx,
			`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, tbl).Scan(&name); err != nil || name != tbl {
			return false
		}
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'proc_start'`).Scan(&n); err != nil {
		return false
	}
	// loto-k5el.2: a v9 DB carrying the old single-column-PK / no-mode locks
	// table is NOT fully current — without these probes migrate's fast path
	// would skip ensureLocksModeAndPK and the rebuild would never run.
	var modeN int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'mode'`).Scan(&modeN); err != nil {
		return false
	}
	var pkCols int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		return false
	}
	// events CHECK must already admit lock_downgraded, else ensureEventsCheckCurrent
	// still has work to do.
	var eventsDDL string
	if err := db.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&eventsDDL); err != nil {
		return false
	}
	return n > 0 && modeN == 1 && pkCols == 2 && strings.Contains(eventsDDL, "lock_downgraded")
}

func (s *Store) Close() error { return s.db.Close() }

// SetStderr overrides the writer used for diagnostic messages (audit-write
// failures, op-flock contention notices). Defaults to os.Stderr. Intended for
// tests that need to observe these messages; production code should keep the
// default.
func (s *Store) SetStderr(w io.Writer) { s.stderr = w }

// beginTx starts an immediate-mode tx on a dedicated pooled conn whose
// busy_timeout PRAGMA is scaled to the caller's ctx deadline. Returned
// cleanup MUST be deferred — it rolls back if Commit wasn't called and
// always releases the conn back to the pool. Rollback after Commit is a
// safe no-op (sql.ErrTxDone), so callers may unconditionally `defer cleanup()`.
//
// Rationale (gh#55): the DSN-level busy_timeout=5000 ignored caller ctx:
// short deadlines couldn't pre-empt SQLite's internal poll loop, and
// longer deadlines were silently truncated to 5s. Per-tx scaling restores
// the contract that ctx is authoritative.
// commitTxFn indirects tx.Commit so tests can simulate a commit failure
// (disk-full / SQLITE_IOERR) on a write path without a real I/O fault.
var commitTxFn = func(tx *sql.Tx) error { return tx.Commit() }

func (s *Store) beginTx(ctx context.Context) (*sql.Tx, func(), error) {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	timeoutMs := txBusyTimeoutMs(ctx, time.Now())
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA busy_timeout=%d", timeoutMs)); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	cleanup := func() {
		_ = tx.Rollback()
		// Reset busy_timeout to the DSN default before the conn returns to
		// the pool — otherwise the next caller inherits this caller's
		// ctx-scaled value (gh#55 follow-up).
		_, _ = conn.ExecContext(context.Background(), fmt.Sprintf("PRAGMA busy_timeout=%d", txBusyTimeoutDefaultMs))
		_ = conn.Close()
	}
	return tx, cleanup, nil
}

// txBusyTimeoutMs maps ctx.Deadline → SQLite busy_timeout in ms.
// No deadline → fall back to DSN default (5000ms).
// Deadline already past → 1ms (caller will see ctx.Err() at next step).
// Otherwise → milliseconds remaining, clamped to [1, txBusyTimeoutCapMs].
func txBusyTimeoutMs(ctx context.Context, now time.Time) int {
	dl, ok := ctx.Deadline()
	if !ok {
		return txBusyTimeoutDefaultMs
	}
	rem := dl.Sub(now).Milliseconds()
	switch {
	case rem < 1:
		return 1
	case rem > txBusyTimeoutCapMs:
		return txBusyTimeoutCapMs
	default:
		return int(rem)
	}
}

const (
	txBusyTimeoutDefaultMs = 5000
	txBusyTimeoutCapMs     = 60000
)

// opFlockPath returns <db-dir>/lock-op.flock — the project-wide op-flock.
func (s *Store) opFlockPath() string {
	return filepath.Join(filepath.Dir(s.dbPath), "lock-op.flock")
}

// migrate applies schema DDL inside a transaction, then sets user_version
// in a separate statement. PRAGMA user_version is not transactional in
// SQLite (it takes effect immediately regardless of tx state), so it runs
// after the DDL tx commits. If a crash occurs between commit and PRAGMA,
// the schema is intact but user_version is stale; openOnce's gate detects
// that (stale version + present `locks` table) and routes the next Open back
// through this idempotent migrate, which re-stamps user_version (loto-vmym).
func (s *Store) migrate(ctx context.Context) error {
	// Steady-state fast path (loto-0gsu): if the DB is already at the current
	// version with an intact schema, do nothing. A redundant migrate here would
	// open an immediate-mode (write) tx that takes SQLite's WAL writer lock and
	// re-stamp user_version on every Open — even for read-only commands
	// (cmdCheck, cmdStatus) that reach openOnce → migrate. That serialized
	// concurrent reads on the writer lock and dirtied the DB on every read.
	// Both probes are read-only PRAGMA/SELECTs on the pool conn — no write tx.
	// The stale-but-intact case (loto-vmym crash window: version below target)
	// falls through and re-migrates in place, re-stamping user_version.
	var v int
	if err := s.db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&v); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	if v == schemaUserVersion && schemaFullyCurrent(ctx, s.db) {
		return nil
	}

	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin migrate tx: %w", err)
	}
	defer cleanup()
	if _, err := tx.ExecContext(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// Additive, in-place column upgrade for DBs created before proc_start
	// existed (loto-kwlp). CREATE TABLE IF NOT EXISTS no-ops on an existing
	// table, so the column is added here instead. Guarded by a table-info
	// probe rather than catching the duplicate-column error, so it stays a
	// no-op on fresh DBs (where CREATE already declared the column) and on
	// every re-Open. user_version is intentionally NOT bumped — bumping would
	// trip the move-aside path and destroy live locks; this upgrade preserves
	// existing rows (their proc_start defaults to NULL = unknown).
	if err := ensureLocksProcStart(ctx, tx); err != nil {
		return fmt.Errorf("add locks.proc_start: %w", err)
	}
	// Composite-PK + mode-column upgrade for DBs that predate loto-k5el.2.
	// SQLite cannot ALTER a primary key in place, so this rebuilds the locks
	// table (guarded — no-op when the PK is already composite) inside the same
	// migrate tx. user_version is intentionally NOT bumped (same rationale as
	// proc_start above). Must run AFTER ensureLocksProcStart so the rebuild's
	// SELECT can rely on proc_start existing on the legacy table.
	if err := ensureLocksModeAndPK(ctx, tx); err != nil {
		return fmt.Errorf("upgrade locks mode/pk: %w", err)
	}
	// Widen the events CHECK for the lock_downgraded kind on pre-loto-k5el.2 DBs
	// (a CHECK can't be ALTERed; rebuild guarded by a DDL substring probe).
	if err := ensureEventsCheckCurrent(ctx, tx); err != nil {
		return fmt.Errorf("upgrade events check: %w", err)
	}
	if err := commitTxFn(tx); err != nil {
		return fmt.Errorf("commit schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, schemaUserVersion)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	return nil
}

// ensureLocksProcStart adds the locks.proc_start column to an existing DB that
// predates it. No-op when the column is already present (fresh DBs declare it
// in CREATE TABLE). Runs inside the migrate tx so a failure rolls back cleanly.
func ensureLocksProcStart(ctx context.Context, tx *sql.Tx) error {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE name = 'proc_start'`,
	).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `ALTER TABLE locks ADD COLUMN proc_start INTEGER`)
	return err
}

// ensureLocksModeAndPK brings a pre-loto-k5el.2 DB up to the composite-PK +
// mode-column shape. SQLite cannot ALTER a primary key in place, so when the PK
// is still the legacy single column the locks table is rebuilt (12-step idiom)
// inside the migrate tx, defaulting every existing row's mode to 'exclusive'
// (preserving the pre-mode binary-lock = sole-writer semantics). user_version is
// intentionally NOT bumped — a bump trips MoveCorruptAside and destroys live
// locks (loto-kwlp precedent). Guarded by a PK-shape probe so this is a no-op on
// fresh DBs (CREATE TABLE already declared the composite PK) and on every re-Open.
func ensureLocksModeAndPK(ctx context.Context, tx *sql.Tx) error {
	var pkCols int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM pragma_table_info('locks') WHERE pk > 0`).Scan(&pkCols); err != nil {
		return err
	}
	if pkCols == 2 {
		return nil // already migrated (fresh DB or prior upgrade)
	}
	// Legacy single-column PK: rebuild. The old table has no `mode` column, so
	// the SELECT supplies the literal 'exclusive' for it. proc_start is present
	// (ensureLocksProcStart ran first), so the column list is valid.
	const rebuild = `
CREATE TABLE locks_new (
  target_canonical TEXT NOT NULL,
  owner_uuid       TEXT NOT NULL,
  session_uuid     TEXT NOT NULL,
  intent           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL,
  expires_at       INTEGER NOT NULL,
  host             TEXT NOT NULL,
  pid              INTEGER NOT NULL,
  proc_start       INTEGER,
  branch           TEXT NOT NULL DEFAULT '',
  mode             TEXT NOT NULL DEFAULT 'exclusive',
  PRIMARY KEY (target_canonical, owner_uuid)
);
INSERT INTO locks_new
  (target_canonical, owner_uuid, session_uuid, intent, created_at,
   expires_at, host, pid, proc_start, branch, mode)
SELECT target_canonical, owner_uuid, session_uuid, intent, created_at,
       expires_at, host, pid, proc_start, branch, 'exclusive'
FROM locks;
DROP TABLE locks;
ALTER TABLE locks_new RENAME TO locks;
CREATE INDEX IF NOT EXISTS idx_locks_target   ON locks(target_canonical);
CREATE INDEX IF NOT EXISTS idx_locks_owner    ON locks(owner_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_session  ON locks(session_uuid);
CREATE INDEX IF NOT EXISTS idx_locks_expires  ON locks(expires_at);`
	_, err := tx.ExecContext(ctx, rebuild)
	return err
}

// ensureEventsCheckCurrent widens the events CHECK constraint to admit the
// lock_downgraded kind on a DB created before loto-k5el.2. A CHECK can't be
// ALTERed, so the events table is rebuilt — but only when the stored DDL lacks
// the new kind (probe via sqlite_master.sql substring), making this a no-op on
// fresh DBs and re-Opens. Runs inside the migrate tx; user_version not bumped.
func ensureEventsCheckCurrent(ctx context.Context, tx *sql.Tx) error {
	var ddl string
	if err := tx.QueryRowContext(ctx,
		`SELECT sql FROM sqlite_master WHERE type='table' AND name='events'`).Scan(&ddl); err != nil {
		return err
	}
	if strings.Contains(ddl, "lock_downgraded") {
		return nil // already current
	}
	const rebuild = `
CREATE TABLE events_new (
  id               TEXT PRIMARY KEY,
  target_canonical TEXT NOT NULL,
  event_kind       TEXT NOT NULL CHECK (event_kind IN ('lock_acquired','lock_released','lock_broken','lock_reclaimed_stale','mode_restore_failed','acquire_rollback_started','lock_downgraded')),
  actor_uuid       TEXT NOT NULL,
  subject_uuid     TEXT,
  reason           TEXT NOT NULL DEFAULT '',
  created_at       INTEGER NOT NULL
);
INSERT INTO events_new (id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, created_at)
SELECT id, target_canonical, event_kind, actor_uuid, subject_uuid, reason, created_at FROM events;
DROP TABLE events;
ALTER TABLE events_new RENAME TO events;
CREATE INDEX IF NOT EXISTS idx_events_target     ON events(target_canonical, created_at);
CREATE INDEX IF NOT EXISTS idx_events_kind       ON events(event_kind, created_at);
CREATE INDEX IF NOT EXISTS idx_events_created_id ON events(created_at, id);`
	_, err := tx.ExecContext(ctx, rebuild)
	return err
}
