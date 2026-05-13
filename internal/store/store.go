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

const schemaUserVersion = 5

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
func Open(p string) (*Store, error) {
	s, err := openOnce(p)
	if err == nil {
		return s, nil
	}
	if !isCorruptDB(err) && !isUserVersionMismatch(err) {
		return nil, err
	}

	release, lockErr := acquireRecoveryLock(p)
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

	moved, mvErr := MoveCorruptAside(p, time.Now())
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

// SetStderr lets tests override the writer used for op-flock wait notices.
func (s *Store) SetStderr(w io.Writer) { s.stderr = w }

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
