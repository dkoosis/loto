# Plan: loto-4n65 — parent-dir fsync gaps (MkdirAll + doctor.go quarantine)

Shape: standard (bug) · Profile: craft · Stacked on `loto-cq6` (PR #149; `syncDir` not yet on main).

## Problem

Follow-up from the loto-cq6 review. Two parent-dir-fsync gaps of the same durability class, deliberately scoped out of cq6:

1. **MkdirAll-created dirs not fsync'd into their parent.**
   `internal/identity/registry.go:269` (`claimSessionCache` → `MkdirAll(sessionDir())`) and
   `:492` (`writeAgent` → `MkdirAll(registryDir())`). When `MkdirAll` *creates* `session/` or
   `agents/`, the new directory entry in the parent (`~/.loto/`) is not fsync'd — power loss can
   drop the dir entry even though the file inside was made durable. Small window (dirs created once
   early, reused), hence P3.

2. **doctor.go corrupt-DB quarantine renames lack parent-dir fsync.**
   `internal/store/doctor.go` `moveCorruptAside`: files renamed *into* staging (`:317`, `:326`) and
   the commit-rename of staging into its parent (`:331`), plus the failure-path requarantine rename
   (`:308`). Recovery path, not the publish hot path — same durability class.

## Approach

Reuse the cq6 `syncDir` pattern (open dir → `Sync()` → close). Best-effort relative to the
operation that already succeeded: the bytes/rename are already durable, so a dir-flush IO error is
*surfaced* (returned) but never retracts a completed claim.

### Architecture constraint
- `internal/store` may depend only on `internal/domain` (`.go-arch-lint.yml`). It cannot import a
  shared fs-helper package. So `syncDir` is duplicated into `internal/store` — the third copy,
  matching cq6's documented identity/cli duplication rationale. Each copy is <12 lines, below jscpd
  `minLines`, so clone detection does not fire (verified by `make audit`).

## Files edited

| File | Change |
|------|--------|
| `internal/identity/registry.go` | Add `mkdirAllSync(dir)` helper (fixed 0o700; `perm` dropped per unparam); use it at the two `MkdirAll` sites and route `newAgent` through `writeAgent` (drop its redundant bare `MkdirAll`). |
| `internal/identity/registry_test.go` | `TestMkdirAllSync`: creates+syncs a missing dir, multi-level create (fresh-home shape), idempotent on an existing dir, surfaces the "not a directory" error when the path is a file. |
| `internal/store/doctor.go` | Add `syncDir` helper; sync `staging` after assembling moved files; sync parent `dir` after commit-rename and after failure-path requarantine. |
| `internal/store/doctor_test.go` | `TestSyncDir` (helper contract) + assert `moveCorruptAside` still produces the quarantine dir (regression for the wired sites). |

### `mkdirAllSync` semantics
```go
// mkdirAllSync is os.MkdirAll(dir, 0o700) plus an fsync of every newly-created
// level's parent, so each new directory entry survives power loss (loto-4n65,
// same class as loto-cq6). A pre-existing directory is a no-op. On a fresh home
// MkdirAll creates more than one level (~/.loto then ~/.loto/agents); we walk
// from dir up to the first existing ancestor and fsync each created level's
// parent. 0o700 is fixed (perm param dropped — unparam: every caller passed it).
func mkdirAllSync(dir string) error {
	if fi, err := os.Stat(dir); err == nil && fi.IsDir() {
		return nil
	}
	var created []string
	for p := dir; ; {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			break
		}
		created = append(created, p)
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	for _, p := range created {
		if err := syncDir(filepath.Dir(p)); err != nil {
			return err
		}
	}
	return nil
}
```
- Stat-shows-dir → skip (MkdirAll would be a no-op; parents unchanged).
- Stat-shows-non-dir or Stat-errors → fall through to MkdirAll, which surfaces the real error
  (preserves original "not a directory" behavior — no error masking).

### doctor.go wiring
- After the sibling-rename loop (`:329`): `syncDir(staging)` so the moved files' entries are durable
  before staging is published. Covers sites `:317`/`:326`.
- After commit-rename success (`:331`): set `committed = true` *first* (so the deferred cleanup
  won't chase a now-renamed staging), then `syncDir(dir)`; a sync error is returned.
- Failure-path defer (`:308`): after a successful requarantine rename, `_ = syncDir(dir)`
  (best-effort — defer can only log, matching the existing recovery tone).

## Testing
Durability across power-loss is not observable from userspace without fault injection (per cq6
`TestSyncDir` rationale). Tests cover the helper's open→sync→close contract and assert the publish
sites still succeed end-to-end. No behavior change to existing happy/error paths.

## Out of scope
- Sharing `syncDir` via a helper package — blocked by the store→domain-only arch rule.

## Addendum (pass-1 review F1)
Original plan scoped out multi-level durability on the assumption loto creates only one missing
level. False: on a fresh home `MkdirAll(~/.loto/agents)` creates two levels, and `newAgent` had a
redundant bare `MkdirAll` that short-circuited the create-path fsync entirely. Fix: `mkdirAllSync`
now fsyncs every created level's parent, and `newAgent` routes through `writeAgent`. See
`pass-1-review.md`.
