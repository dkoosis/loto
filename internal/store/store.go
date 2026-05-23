package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const schemaUserVersion = 8

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
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		return openWithRecovery(ctx, p)
	}

	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return nil, fmt.Errorf("mkdir state dir: %w", err)
	}
	flock, err := acquireOpFlock(ctx, opFlockPathFor(p), os.Stderr)
	if err != nil {
		return nil, err
	}
	defer flock.release()
	return openWithRecovery(ctx, p)
}

// opFlockPathFor returns the op-flock path for a DB at p — used during
// Open() before a *Store exists.
func opFlockPathFor(p string) string {
	return filepath.Join(filepath.Dir(p), "lock-op.flock")
}

func openWithRecovery(ctx context.Context, p string) (*Store, error) {
	s, err := openOnce(p)
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
	if s2, err2 := openOnce(p); err2 == nil {
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
	return openOnce(p)
}

func openOnce(p string) (*Store, error) {
	preExisted := false
	if st, err := os.Stat(p); err == nil && st.Size() > 0 {
		preExisted = true
	}

	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}

	if preExisted {
		var v int
		if err := db.QueryRowContext(context.Background(), `PRAGMA user_version`).Scan(&v); err != nil {
			db.Close()
			return nil, fmt.Errorf("read user_version: %w", err)
		}
		if v != schemaUserVersion {
			db.Close()
			return nil, fmt.Errorf("%w: have %d, want %d", errUserVersionMismatch, v, schemaUserVersion)
		}
	}

	s := &Store{db: db, dbPath: p, stderr: os.Stderr}
	if err := s.migrate(); err != nil {
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

func (s *Store) Close() error { return s.db.Close() }

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

func (s *Store) migrate() error {
	if _, err := s.db.ExecContext(context.Background(), schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
