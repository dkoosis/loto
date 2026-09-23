# Modern Go audit

Date: 2026-09-23  
Scope: all 274 Go files in `./...`  
Language version: Go 1.26.4 (`go.mod`)

## Guideline source and method

The requested JetBrains repository could not be cloned: every GitHub endpoint
available in this environment returned HTTP 403 from the network proxy. The
`use-modern-go` skill and its `list`/`explain` commands were therefore not
available, and no downloaded guideline or tool content is included here. This
is an explicit audit limitation rather than a substituted or invented result.

As the closest version-aware check, the Go 1.26.4 toolchain's complete
modernization fixer suite was run over `./...` with `go fix -diff`. It evaluated
these guideline/analyzer IDs: `any`, `fmtappendf`, `forvar`, `mapsloop`,
`minmax`, `newexpr`, `omitzero`, `plusbuild`, `rangeint`, `reflecttypefor`,
`slicescontains`, `slicessort`, `stditerators`, `stringsbuilder`, `stringscut`,
`stringscutprefix`, `stringsseq`, `testingcontext`, and `waitgroup`. A manual
repository-wide search also reviewed legacy sorting APIs.

## Findings and changes

| ID | Finding | Disposition |
| --- | --- | --- |
| `slicessort` | The gate package used `sort.Strings` for nine string-slice sorts even though the generic `slices.Sort` API is available at the repository's Go version. | Changed all nine gate-package calls. Sorting remains in-place and ascending, so behavior is preserved. |
| all other IDs above | The version-aware fixer reported no applicable rewrites anywhere in `./...`. | No change. |

No source change was made without a finding in this table.

## Findings not changed

The same `slicessort` concern exists outside `internal/gate` (26 calls to
`sort.Strings`, including tests). Those occurrences are intentionally left for
a follow-up so this does not become one giant modernization PR. Uses of
`sort.Slice`/`sort.SliceStable` also remain: converting them to
`slices.SortFunc`/`slices.SortStableFunc` requires comparator-by-comparator
review and belongs in a separate change.

The JetBrains-specific audit remains incomplete until its repository can be
fetched. In particular, its own `list` output and `explain` text must be checked
before treating the analyzer IDs above as a complete list of JetBrains
applicable guidelines.

## Proposed PR breakdown

1. **This PR:** replace legacy string sorting only in `internal/gate`.
2. Replace remaining `sort.Strings` calls package by package, with focused tests.
3. Review comparator sorts for safe `slices.SortFunc` or
   `slices.SortStableFunc` conversion.
4. Once network access is available, rerun the complete JetBrains `list` and
   `explain` workflow and address any non-overlapping findings in small,
   concern-specific PRs.

## Validation

- `gofmt` was run on every changed Go file.
- `go fix -diff ./...` completed with no suggested edits.
- `go test ./internal/gate` and repository-wide `go vet ./...` passed.
- `go test ./...` reached the script suite but failed because the test fixture did not install/route the repository reference-transaction hook; three guard tests observed an unexpectedly successful `git checkout`. The changed `internal/gate` package passed.
- `git diff --check` passed.
- The repository-wide locked lint could not run because the pinned linter was
  not cached and the network proxy rejected its Go module download.
