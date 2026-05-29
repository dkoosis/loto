# Final state — loto-cq6 (gh#131)

## Summary
Added a `syncDir(dir)` helper that fsyncs a directory fd, and wired it into the
three durable-write sites the bead names so a new dirent survives power loss:
- `writeAgent` (registry.go) — after rename; returns the error (fresh publish).
- `claimSessionCache` (registry.go) — after O_EXCL create+close; best-effort
  (swallowed) because the claim already won and the caller treats non-ErrExist
  as fatal. Reviewer confirmed returning here would be a bug.
- `pinSlug` (paths.go) — added the missing temp `Sync` + dir sync; best-effort.

Helper is duplicated (identity + cli) rather than shared: `internal/identity → ∅`
is a hard arch-lint invariant. ~9 lines, under jscpd's 12-line/100-token floor →
`make dupl` reports 0 clones.

## Shape / profile
standard · craft · authority: ship (project norm is PR-per-bead, #133–147)

## Verification
- `make audit` green — vet, lint, arch, race, govulncheck (0), jscpd (0 clones),
  nilaway, demo. Log: stage-0-audit-clean.log
- New `TestSyncDir` (helper contract). Durability-across-crash not unit-testable
  without fault injection — stated, not faked. Regression coverage from existing
  TestWriteAgentAtomic + TestEnsureSessionCachePersists + slug-pin tests.

## Review
1 persistence specialist, adversarial. 0 findings against the diff; 2 deferred
(see triage.md).

## Findings
- Applied: 0
- Rejected: 0
- Deferred: 2 → **loto-4n65** (MkdirAll grandparent sync + doctor.go quarantine)
- Escalated to dk: 0

## Plan-vs-actual delta
- Plan dropped `internal/fsx` (arch invariant) → duplicated unexported helper. Documented.
- Files changed exactly as revised plan: registry.go, registry_test.go, paths.go. No out-of-plan files.

## north_star_answer
(not collected — small contained bug, no direction question raised)
