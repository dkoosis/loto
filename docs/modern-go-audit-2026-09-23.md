# Modern Go audit (2026-09-23)

## Scope and method

The audit covered every tracked `*.go` file in the module. The module targets Go
1.26.4. The JetBrains Modern Go Guidelines CLI was run with `list --file-path
go.mod`; detailed guidance was then requested for each candidate family below.
The downloaded guidelines and CLI remain outside the repository.

## Applicable guidelines and findings

| Guideline ID | Finding | This change |
| --- | --- | --- |
| `errors_as_type` | Production code contained 11 pointer-target `errors.As` checks; tests contain 25 more. | Converted the 11 production checks to `errors.AsType`. |
| `testing_t_context` | Test code contains 532 uses of `context.Background()`/`TODO()`. Many are candidates for `t.Context()`, while cancellation, subprocess-lifetime, and context-fallback tests intentionally need independent roots. | Not changed; the volume and required lifetime review merit a test-only PR. |
| `sync_waitgroup_go` | Concurrency tests use explicit `Add`/`go`/`Done` worker triplets. Barrier wait groups (`ready` and `start`) do not fit `WaitGroup.Go`; worker `done` groups do. | Not changed; isolate the worker conversion from production error handling. |
| `slices_sort` / `slices_sort_func` | Production code has 98 calls to legacy `sort` helpers. Straight ordered sorts fit `slices.Sort`; comparator sorts require typed conversions, and stable sorts must retain stability. | Not changed; this is a broad mechanical concern best split by package and independently reviewed. |
| `json_omitzero` | Seven numeric JSON fields use `omitempty`; their wire behavior should remain the same with `omitzero`. String and slice uses correctly remain `omitempty`. | Not changed; keep serialization changes in their own compatibility-focused PR. |

No actionable uses were found for `interface{}`, old benchmark `b.N` loops,
manual `time.Now().Sub`, reflective pointer type discovery, or iteration over
materialized `strings.Split`/`Fields` results. Existing code already uses `any`,
`b.Loop`, `time.Since`, `reflect.TypeFor`, `SplitSeq`, `FieldsSeq`, integer
ranges, and other applicable modern forms. Other listed guidelines did not fit
the code encountered or would change semantics (for example, cancellation
causes where callers do not inspect a cause).

## Changes made

This first, deliberately narrow PR updates production error-type inspection to
`errors.AsType`. It preserves wrapped-error matching and existing branches while
removing pointer-target temporaries. Test-only occurrences are intentionally
left for the follow-up test modernization PR.

## Proposed PR breakdown

1. **Production error matching (this PR):** apply `errors_as_type` to non-test
   code and add this audit record.
2. **Test modernization:** apply `errors_as_type`, `testing_t_context`, and
   `sync_waitgroup_go` in tests after classifying context and barrier lifetimes.
3. **Typed sorting:** migrate legacy sorting package by package, preserving
   stable ordering where required.
4. **JSON omission semantics:** migrate numeric fields to `omitzero` with
   golden/round-trip compatibility coverage.

This separation keeps each review coherent and avoids one giant modernization
change.

## Validation

- `gofmt` was run on every changed Go file.
- `go vet` and repository lint/architecture checks passed through `make check`.
- `make check` reached the complete Go test suite, where three
  reference-transaction script cases unexpectedly allowed a checkout and
  `TestRunVerifyCmdKillsOrphanedGrandchild` observed a surviving child process.
  Focused reruns reproduced both environment/process-sensitive failures; the
  other reported packages passed, including every package changed here except
  that unrelated `internal/lane` process-lifetime case.
- Formatting, documentation, script, Makefile, and SDLC validation were
  requested through the repository check target; its test failure stopped the
  later validation prerequisites.
- A final search verified that `errors.As` remains only in test files.
