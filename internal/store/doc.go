// Package store is the SQLite adapter for loto's tag layer.
//
// # Scope: tags, not locks
//
// This package persists the descriptive half of lock-out / tag-out — who
// claimed a target, when, with what stated intent, and any messages from
// blocked peers. It does not enforce exclusion. A row in the locks table
// is a tag in the LOTO sense: it explains, it does not impede.
//
// Per docs/NORTH_STAR.md the enforcement half is flock(2) on a per-file
// .lock sidecar (and a global.lock for sweeps). flock is process-bound
// kernel state; this package never tries to model it. Foreground holds
// are flock-authoritative; this package's rows are authoritative only for
// the record-tier carve-out (a tag with non-zero, unexpired ExpiresAt
// describes a claim that persists across the PreToolUse → PostToolUse
// hook gap, where no foreground process can hold flock).
//
// If you find yourself reasoning about whether a row prevents a write,
// you are in the wrong layer — the row describes the claim, the kernel
// (or file mode) prevents the write.
//
// # Why SQLite
//
// Atomic multi-row updates (lock + system tag + cursor in one BEGIN
// IMMEDIATE), a single fsync per transaction, and zero daemon. Identity
// resolution, overlap detection, and case-sensitivity probing all live
// in adjacent files. Schema is in schema.sql, applied idempotently on
// every Open() — no migrator.
package store
