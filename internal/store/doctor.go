package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"loto/internal/domain"
)

const (
	sqliteWALSuffix = "-wal"
	sqliteSHMSuffix = "-shm"
)

type DoctorReport struct {
	StaleLocks      []domain.LockRecord
	SidecarFindings []SidecarFinding
	IntegrityOK     bool
	IntegrityDetail string
}

// SidecarCheck cross-checks held locks against the CC session sidecar to
// strengthen zombie detection. SidecarDir empty disables the check; RepoTop
// empty disables cwd-mismatch detection (no-sidecar still fires). The caller
// is the runtime layer which knows the on-disk paths.
type SidecarCheck struct {
	SidecarDir string
	RepoTop    string
}

func (s *Store) DoctorAuditWith(ctx context.Context, thisHost string, live domain.PidLiveProbe, sc SidecarCheck) (*DoctorReport, error) {
	r := &DoctorReport{}
	locks, err := s.ListLocks(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range locks {
		if domain.IsStale(locks[i], now, thisHost, live) {
			r.StaleLocks = append(r.StaleLocks, locks[i])
			continue
		}
		if locks[i].Host == thisHost && sc.SidecarDir != "" {
			if f, ok := checkSidecar(locks[i], sc); ok {
				r.SidecarFindings = append(r.SidecarFindings, f)
			}
		}
	}
	rows, err := s.db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	r.IntegrityOK = len(lines) == 1 && lines[0] == "ok"
	if r.IntegrityOK {
		r.IntegrityDetail = "ok"
	} else {
		r.IntegrityDetail = strings.Join(lines, "; ")
	}
	return r, nil
}

// checkSidecar inspects ~/.claude/sessions/<pid>.json for a held lock and
// returns a finding when the sidecar is missing or its cwd doesn't match the
// repo the lock lives in. Indeterminate errors (e.g. permission denied) are
// silenced — they're not actionable zombie signals.
func checkSidecar(l domain.LockRecord, sc SidecarCheck) (SidecarFinding, bool) {
	got, err := readSidecar(sc.SidecarDir, l.PID)
	if err != nil {
		if sidecarMissing(err) {
			return SidecarFinding{
				PID:    l.PID,
				Target: l.Target.Canonical,
				Reason: SidecarReasonNoSidecar,
			}, true
		}
		return SidecarFinding{}, false
	}
	if sc.RepoTop == "" || got.CWD == "" {
		return SidecarFinding{}, false
	}
	if got.CWD != sc.RepoTop {
		return SidecarFinding{
			PID:    l.PID,
			Target: l.Target.Canonical,
			Reason: SidecarReasonCwdMismatch,
			Detail: got.CWD,
		}, true
	}
	return SidecarFinding{}, false
}

func (s *Store) DoctorRepair(ctx context.Context, thisHost, byAgent string, live domain.PidLiveProbe) error {
	tx, cleanup, err := s.beginTx(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	all, err := loadLocksTx(ctx, tx)
	if err != nil {
		return err
	}
	now := time.Now()
	var reclaimed []string
	for i := range all {
		if domain.IsStale(all[i], now, thisHost, live) {
			if err := reclaimStaleTx(ctx, tx, all[i], byAgent, now); err != nil {
				return err
			}
			reclaimed = append(reclaimed, all[i].Target.Canonical)
		}
	}
	if err := rotateEventsTx(ctx, tx, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, p := range reclaimed {
		s.restoreAndAudit(ctx, p, byAgent)
	}
	_, err = s.db.ExecContext(ctx, `VACUUM`)
	return err
}

// ScanOrphanModes returns paths that are read-only on disk but have no
// matching lock row. Caller supplies the candidate paths (typically all
// regular files under the repo, or a curated subset).
func (s *Store) ScanOrphanModes(ctx context.Context, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT target_canonical FROM locks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	owned := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		owned[c] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var orphans []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !st.Mode().IsRegular() {
			continue
		}
		if st.Mode().Perm()&0o222 != 0 {
			continue
		}
		if owned[p] {
			continue
		}
		orphans = append(orphans, p)
	}
	sort.Strings(orphans)
	return orphans, nil
}

// RestoreOrphanMode chmods the given paths back to owner-writable. Caller
// gates this behind explicit user intent (--restore-orphan-mode). Returns
// the paths it successfully chmod'd and a parallel slice of per-path failures
// so callers can surface why a file was skipped.
func (s *Store) RestoreOrphanMode(paths []string) (restored []string, failures []OrphanRestoreFailure) {
	for _, p := range paths {
		if err := restoreWrite(p); err != nil {
			failures = append(failures, OrphanRestoreFailure{Path: p, Err: err})
			continue
		}
		restored = append(restored, p)
	}
	return restored, failures
}

// OrphanRestoreFailure pairs an orphan-mode path with the error encountered
// while trying to chmod it back to writable.
type OrphanRestoreFailure struct {
	Path string
	Err  error
}

// moveCorruptAside relocates a corrupt DB and its -wal/-shm siblings into
// a single sibling directory <dbPath>.corrupt.<RFC3339Z>/. The move is
// atomic: files are first assembled in a staging directory, which is then
// renamed into place with one os.Rename. This eliminates the race in the
// previous three-rename approach, where a concurrent opener could see a
// fresh main DB paired with a stale sidecar (gh#48).
func moveCorruptAside(dbPath string, when time.Time) (string, error) {
	dir := filepath.Dir(dbPath)
	base := filepath.Base(dbPath)
	stamp := when.UTC().Format("2006-01-02T15-04-05Z")
	finalDir := fmt.Sprintf("%s.corrupt.%s", dbPath, stamp)

	staging, err := os.MkdirTemp(dir, base+".corrupt-staging-")
	if err != nil {
		return "", fmt.Errorf("make staging dir: %w", err)
	}
	// holdsCorruptBytes flips true after the first os.Rename(dbPath, …) — at
	// that point staging contains the only copy of the user's corrupt DB.
	// Wiping it on later failure (the original RemoveAll defer) is data loss
	// the user can't recover from (audit loto-2y6). Instead, requarantine the
	// staging dir under a *.corrupt.failed.<stamp>/ name so the forensic bytes
	// survive even when the final commit-rename can't proceed.
	holdsCorruptBytes := false
	committed := false
	defer func() {
		if committed {
			return
		}
		if !holdsCorruptBytes {
			_ = os.RemoveAll(staging)
			return
		}
		failed := fmt.Sprintf("%s.corrupt.failed.%s", dbPath, stamp)
		if err := os.Rename(staging, failed); err != nil {
			// Last resort: leave staging in place. The dir name still encodes
			// "corrupt-staging-" so it's discoverable even unrenamed.
			fmt.Fprintf(os.Stderr, "loto: corrupt DB bytes preserved at %s (rename to %s failed: %v)\n", staging, failed, err)
		} else {
			fmt.Fprintf(os.Stderr, "loto: corrupt DB bytes preserved at %s after commit-rename failure\n", failed)
		}
	}()

	if err := os.Rename(dbPath, filepath.Join(staging, base)); err != nil {
		return "", fmt.Errorf("rename main: %w", err)
	}
	holdsCorruptBytes = true
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
