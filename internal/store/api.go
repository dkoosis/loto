// Package-level role interfaces. *Store satisfies all of them; callers
// should depend on the narrowest interface they need (ISP).
//
// EventLog is intentionally omitted — its only consumer today is *Store
// itself (lock ops emit audit events transactionally). Add it here when
// an external consumer (inspect/inbox UI) appears.

package store

import (
	"context"

	"loto/internal/domain"
)

// LockOps is the lock-table contract consumed by cmd_lock, cmd_unlock,
// cmd_check, cmd_status, and the doctor's lock-survey path.
type LockOps interface {
	AcquireLocks(ctx context.Context, recs []domain.LockRecord, live domain.PidLiveProbe) ([]domain.LockRecord, error)
	ReleaseLocks(ctx context.Context, targets []domain.Target, byAgent string) ([]ReleaseResult, error)
	BreakLocks(ctx context.Context, targets []domain.Target, byAgent string, mode BreakMode, reason string, live domain.PidLiveProbe) ([]BreakResult, error)
	ListLocks(ctx context.Context) ([]domain.LockRecord, error)
	LockAt(ctx context.Context, t domain.Target) (*domain.LockRecord, error)
}

// Health is the audit/repair contract consumed by cmd_doctor.
type Health interface {
	DoctorAuditWith(ctx context.Context, thisHost string, live domain.PidLiveProbe, sc SidecarCheck) (*DoctorReport, error)
	DoctorRepair(ctx context.Context, thisHost, byAgent string, live domain.PidLiveProbe) error
	ScanOrphanModes(ctx context.Context, paths []string) ([]string, error)
	RestoreOrphanMode(paths []string) (restored []string, failures []OrphanRestoreFailure)
}

var (
	_ LockOps = (*Store)(nil)
	_ Health  = (*Store)(nil)
)
