package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strconv"
)

func (s *Store) FSCaseSensitive(probeDir string) (bool, error) {
	const key = "fs_case_sensitive"
	var v string
	err := s.db.QueryRowContext(context.Background(), `SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&v)
	if err == nil {
		b, perr := strconv.ParseBool(v)
		return b, perr
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	probed, err := probeFSCase(probeDir)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(context.Background(), `INSERT OR REPLACE INTO schema_meta(key,value) VALUES (?,?)`, key, strconv.FormatBool(probed))
	return probed, err
}

func probeFSCase(dir string) (bool, error) {
	low := filepath.Join(dir, ".loto-case-probe")
	up := filepath.Join(dir, ".LOTO-CASE-PROBE")
	if err := os.WriteFile(low, []byte("x"), 0o600); err != nil {
		return false, err
	}
	defer os.Remove(low)
	stLow, err := os.Stat(low)
	if err != nil {
		return false, err
	}
	stUp, err := os.Stat(up)
	if err != nil {
		return true, nil
	}
	return !os.SameFile(stLow, stUp), nil
}
