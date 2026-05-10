package store

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct {
	db *sql.DB
}

// connDSN: WAL + busy_timeout + immediate-mode write txns.
func connDSN(path string) string {
	return path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_txlock=immediate"
}

func Open(p string) (*Store, error) {
	s, err := openOnce(p)
	if err == nil {
		return s, nil
	}
	if !isCorruptDB(err) {
		return nil, err
	}
	moved, mvErr := MoveCorruptAside(p, time.Now())
	if mvErr != nil {
		return nil, fmt.Errorf("corrupt DB and move-aside failed: %v (orig: %w)", mvErr, err)
	}
	fmt.Fprintf(os.Stderr, "loto: corrupt DB moved aside to %s; creating fresh DB\n", moved)
	return openOnce(p)
}

func openOnce(p string) (*Store, error) {
	db, err := sql.Open("sqlite", connDSN(p))
	if err != nil {
		return nil, fmt.Errorf("sql.Open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func isCorruptDB(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "database disk image is malformed") ||
		strings.Contains(msg, "file is not a database") ||
		strings.Contains(msg, "database is corrupt")
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}
