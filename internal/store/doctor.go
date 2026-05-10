package store

import (
	"context"
	"fmt"
	"os"
	"time"

	"loto/internal/domain"
)

type DoctorReport struct {
	StaleLocks      []domain.LockRecord
	ExpiredTagCount int
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
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tags WHERE expires_at IS NOT NULL AND expires_at < ?`, now.UnixNano()).Scan(&r.ExpiredTagCount); err != nil {
		return nil, err
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM tags WHERE expires_at IS NOT NULL AND expires_at < ?`, now.UnixNano()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// MoveCorruptAside renames a corrupt DB file (and its WAL/SHM siblings) to
// loto.db.corrupt.<RFC3339Z> and lets the next Open() create a fresh DB.
func MoveCorruptAside(dbPath string, when time.Time) (string, error) {
	stamp := when.UTC().Format("2006-01-02T15-04-05Z")
	dst := fmt.Sprintf("%s.corrupt.%s", dbPath, stamp)
	if err := os.Rename(dbPath, dst); err != nil {
		return "", err
	}
	for _, sfx := range []string{"-wal", "-shm"} {
		_ = os.Rename(dbPath+sfx, dst+sfx)
	}
	return dst, nil
}
