package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"loto/internal/domain"
)

const (
	sqliteWALSuffix = "-wal"
	sqliteSHMSuffix = "-shm"
)

type DoctorReport struct {
	StaleLocks      []domain.LockRecord
	IntegrityOK     bool
	IntegrityDetail string
}

func (s *Store) DoctorAudit(ctx context.Context, thisHost string, live domain.PidLiveProbe) (*DoctorReport, error) {
	r := &DoctorReport{}
	locks, err := s.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range locks {
		if domain.IsStale(locks[i], now, thisHost, live) {
			r.StaleLocks = append(r.StaleLocks, locks[i])
		}
	}
	var detail string
	if err := s.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&detail); err != nil {
		return nil, err
	}
	r.IntegrityDetail = detail
	r.IntegrityOK = detail == "ok"
	return r, nil
}

func (s *Store) DoctorRepair(ctx context.Context, thisHost, byAgent string, live domain.PidLiveProbe) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return err
	}
	now := time.Now()
	for i := range all {
		if domain.IsStale(all[i], now, thisHost, live) {
			if err := reclaimStaleTx(ctx, tx, all[i], byAgent, now); err != nil {
				return err
			}
		}
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// MoveCorruptAside relocates a corrupt DB and its -wal/-shm siblings into
// a single sibling directory <dbPath>.corrupt.<RFC3339Z>/. The move is
// atomic: files are first assembled in a staging directory, which is then
// renamed into place with one os.Rename. This eliminates the race in the
// previous three-rename approach, where a concurrent opener could see a
// fresh main DB paired with a stale sidecar (gh#48).
func MoveCorruptAside(dbPath string, when time.Time) (string, error) {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	stamp := when.UTC().Format("2006-01-02T15-04-05Z")
	finalDir := fmt.Sprintf("%s.corrupt.%s", dbPath, stamp)

	staging, err := os.MkdirTemp(dir, base+".corrupt-staging-")
	if err != nil {
		return "", fmt.Errorf("make staging dir: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.RemoveAll(staging)
		}
	}()

	if err := os.Rename(dbPath, filepath.Join(staging, base)); err != nil {
		return "", fmt.Errorf("rename main: %w", err)
	}
	for _, sfx := range []string{sqliteWALSuffix, sqliteSHMSuffix} {
		src := dbPath + sfx
		if _, statErr := os.Stat(src); statErr != nil {
			continue
		}
		if err := os.Rename(src, filepath.Join(staging, base+sfx)); err != nil {
			return "", fmt.Errorf("rename %s: %w", sfx, err)
		}
	}

	if err := os.Rename(staging, finalDir); err != nil {
		return "", fmt.Errorf("commit corrupt dir: %w", err)
	}
	committed = true
	return finalDir, nil
}
