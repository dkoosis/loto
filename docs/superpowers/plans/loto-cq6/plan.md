# Plan — loto-cq6: atomic-rename publishes don't fsync parent dir (gh#131)

## Problem
Three sites write a file durably (temp→fsync→rename, or O_EXCL→fsync) but never
fsync the *parent directory*. On most Unix filesystems an fsync'd file's
directory entry is not itself durable until the containing dir is fsync'd —
a power loss between rename and the dir's metadata hitting disk can leave the
new name unrecoverable even though the bytes were synced.

## Sites (bead-scoped — exactly three)
| Site | File:line | Current tail | Fix |
|------|-----------|--------------|-----|
| writeAgent | `internal/identity/registry.go:510` | `return os.Rename(tmpName, final)` | rename, then `SyncDir(dir)` |
| session sentinel | `internal/identity/registry.go:264` `claimSessionCache` | `return f.Close()` | close, then `SyncDir(sessionDir())` |
| .loto-slug pin | `internal/cli/paths.go:91` | `_ = os.Rename(tmpName, pinFile)` | add `tmp.Sync()` (best-effort) + `_ = SyncDir(dir)` |

Note on the slug pin: it currently never `Sync`s the temp before rename, so a
parent-dir fsync alone would be meaningless (content may not be on disk). Adding
both keeps it best-effort (errors still swallowed) but actually durable.

## Out of scope (deferred)
`internal/store/doctor.go` rename sites (corrupt-DB quarantine, lines 308/317/326/331)
also lack parent-dir fsync. They are recovery-path, not the publish hot path the
bead names. → flag to dk at approval; file a follow-up bead if wanted. Not folded
in (D9, no "while I'm here").

## Design
Arch constraint (`.go-arch-lint.yml`): **`internal/identity → ∅`** — identity may
import no internal package. A shared `internal/fsx` imported by identity would
break `make arch` and the documented leaf invariant. Not worth it for a fsync
helper.

So: unexported `syncDir` duplicated in each package (`identity`, `cli`):
```go
func syncDir(dir string) error {
    d, err := os.Open(dir)
    if err != nil {
        return err
    }
    if err := d.Sync(); err != nil {
        d.Close()
        return err
    }
    return d.Close()
}
```
~9 lines, well under jscpd's `minLines 12` / `minTokens 100` → `make dupl` won't
flag it. Keeps the identity-leaf invariant intact; no arch-lint change.

Error handling per site:
- writeAgent / claimSessionCache: return the SyncDir error (matches existing
  `tmp.Sync`→return style). For claimSessionCache the O_EXCL claim already won;
  a SyncDir error surfaces as claim failure, caller falls back to re-read of the
  file that exists — acceptable, IO-error-only path.
- slug pin: swallow (`_ =`) — best-effort by design.

## Files
| File | Change |
|------|--------|
| `internal/identity/registry.go` | add `syncDir`; wire writeAgent + claimSessionCache |
| `internal/identity/registry_test.go` | `TestSyncDir` — success on tempdir, error on missing path |
| `internal/cli/paths.go` | add `syncDir`; wire slug pin (+temp Sync) |

## Tests / verification
- `TestSyncDir`: nil on a real dir; error on a nonexistent path.
- Parent-dir fsync durability is **not observable from userspace** without
  fault injection (crash between rename and dir-metadata flush). No fake
  durability test. Regression coverage comes from existing
  `TestWriteAgentAtomic`, `TestEnsureSessionCachePersists`, and the slug-pin
  tests — they confirm the added Sync calls don't break publish/claim.
- Gate: `make audit` green.

## TDD order
1. RED: `TestSyncDir` against not-yet-existing `fsx.SyncDir`.
2. GREEN: write `SyncDir`.
3. Wire the three call sites; run existing identity + cli tests.
4. `make audit`.
