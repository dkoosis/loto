# loto-4n65 review passes

## Pass A — plan adherence (feature-dev:code-reviewer)
- All "Files edited" items delivered; `fillCorruptStaging` extraction judged in-scope (gocognit fix, logic unchanged).
- P2: plan.md `mkdirAllSync` snippet still shows old `(dir, perm)` signature; code dropped `perm` (unparam lint). → update plan snippet.
- Otherwise clean.

## Pass B — persistence/durability (go-bug-audit:pass3-persistence)
doctor.go: **clean** across all concerns —
- F4 fillCorruptStaging failure-preservation invariant holds (holdsCorruptBytes=true on every post-main-rename error path; caller assigns before err check). ✓
- F5 committed=true before syncDir(dir); returning syncDir error while committed is correct (no data loss, defer no-ops). ✓
- F6 syncDir(staging) + syncDir(dir) sufficient (renamed inodes already on disk). ✓
- F7 best-effort `_ = syncDir(dir)` in failure defer acceptable. ✓

registry.go: **one real defect** —
- **F1 (P2): mkdirAllSync incomplete on fresh home.**
  - `newAgent:351` calls bare `os.MkdirAll(registryDir())` then `writeAgent(a)`. writeAgent ALSO creates the dir via mkdirAllSync → `:351` is redundant AND defeats the create-path fsync (mkdirAllSync Stat short-circuits to no-op because :351 already made the dir).
  - On fresh home, `MkdirAll(~/.loto/agents)` creates TWO levels; mkdirAllSync only fsyncs the immediate parent (`~/.loto`), never `~` for the `~/.loto` entry. Comment "never creates more than one level here" is **factually false**.
  - Low impact (identity re-mintable, single first-boot window) but should not ship with a false comment + create-path no-op.
- F2 (TOCTOU Stat→MkdirAll) benign. F3 (skip fsync when exists) correct.

### Fix plan for F1 (next session)
1. Remove redundant `os.MkdirAll(registryDir(), 0o700)` at `newAgent:351` — writeAgent's mkdirAllSync handles it (net code reduction, routes create path through the fsync).
2. Upgrade `mkdirAllSync` to fsync every newly-created level (walk from first pre-existing ancestor down) so fresh-home 2-level create is fully durable. Fix the comment.
3. Re-run `make audit`; update plan.md snippet (Pass A P2).

## Pass C — gemini-code-assist (PR #151 review)
Both findings verified against the code and applied:
- **G1: fsync order.** mkdirAllSync flushed parents deepest-first (bottom-up). On a crash mid-walk that can persist a level's contents before its link from the parent — an orphaned inode. Reversed to top-down (shallowest parent first) via `slices.Backward`.
- **G2: TOCTOU in fillCorruptStaging.** WAL/SHM quarantine used `os.Stat(src)`-then-`os.Rename`. Window is narrow here (main DB already moved aside, sqlite not active) but the stat is a real race and a redundant syscall. Replaced with a direct rename tolerating `os.IsNotExist`.
- F2 above (Pass B, TOCTOU Stat→MkdirAll in mkdirAllSync) left as-is: benign — MkdirAll is idempotent and surfaces the real error on a racing non-dir create.
