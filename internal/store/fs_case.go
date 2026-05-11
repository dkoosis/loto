package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	// INSERT OR IGNORE: first concurrent writer wins; subsequent writers
	// must re-read the canonical value rather than overwrite it. Combined
	// with per-call unique probe filenames, this makes the cached answer
	// deterministic under concurrent probes (gh#49).
	if _, err := s.db.ExecContext(context.Background(),
		`INSERT OR IGNORE INTO schema_meta(key,value) VALUES (?,?)`,
		key, strconv.FormatBool(probed)); err != nil {
		return false, err
	}
	var canonical string
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&canonical); err != nil {
		return false, err
	}
	return strconv.ParseBool(canonical)
}

// probeFSCase writes a per-call unique probe file then checks whether the
// uppercase-named variant stats to the same inode. Filename uniqueness
// (pid+random suffix) prevents the gh#49 race where two concurrent probes
// shared one filename and could observe each other's defer-cleanup
// mid-Stat, caching the wrong answer for the DB lifetime.
func probeFSCase(dir string) (bool, error) {
	suffix, err := uniqueProbeSuffix()
	if err != nil {
		return false, fmt.Errorf("probe suffix: %w", err)
	}
	lowName := ".loto-case-probe-" + suffix
	upName := strings.ToUpper(lowName)
	low := filepath.Join(dir, lowName)
	up := filepath.Join(dir, upName)
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

func uniqueProbeSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(b[:])), nil
}
