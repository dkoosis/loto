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
		// Existing-DB path: no create race possible, so op-flock isn't
		// needed. openWithRecovery may take recovery-lock alone.
		return openWithRecovery(ctx, p)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("stat db path: %w", err)
	}

	// Fresh-DB path: op-flock guards the create-race window, but is
	// released before any recovery-lock acquire (gh#109).
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	flock, err := acquireOpFlock(ctx, opFlockPathFor(p), os.Stderr)
	if err != nil {
		return nil, err
	}
	s, openErr := openOnce(ctx, p)
	// Release op-flock BEFORE any recovery-lock acquire. If openOnce
	// succeeded the create race is resolved; if it failed with corruption
	// or version mismatch, openWithRecovery will retake recovery-lock
	// alone — never with op-flock held.
	flock.release()
	if openErr == nil {
		return s, nil
	}
	if !isCorruptDB(openErr) && !isUserVersionMismatch(openErr) {
		return nil, openErr
	}
	return openWithRecovery(ctx, p)
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

func openOnce(ctx context.Context, p string) (*Store, error) {
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
	if err := tx.Commit(); err != nil {
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
