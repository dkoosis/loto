# Codex Sandbox Notes — loto

Operational notes for the OpenAI Codex cloud sandbox running against this repo.

## What loto is (one paragraph)

loto coordinates concurrent Claude/Codex agents editing the same repo. flock(2)
is the source of truth; JSON tags are advisory metadata. Single-host only — no
NFS, no daemon. See `docs/NORTH_STAR.md` for the full design.

## Definition of "done" in sandbox

Sandbox = fast iteration. Ship claims bounded by what the sandbox can prove.

- **OK here**: targeted tests, small refactors, static reasoning, unit-test
  authoring, single-package validation.
- **Defer to CI / local**: cross-platform claims (loto has linux + darwin
  flock variants — both must pass), multi-process integration tests using
  the loto CLI binary, anything touching real `flock(2)` semantics under
  concurrent load.

Rule of thumb: `make check` (vet + test + build) is sufficient evidence to
ship from sandbox. The cross-platform flock_unix.go vs flock_other.go split
is CI's job (GitHub Actions, beads `loto-7wp.2`).

## Canonical commands

| Want                        | Run                          |
| --------------------------- | ---------------------------- |
| Fast validation             | `make check` (fmt+vet+test+build) |
| Race-detector tests         | `make test`                  |
| Doctor (toolchain check)    | `make doctor`                |
| Build the CLI               | `make build`                 |
| Optional lint               | `make lint` (skips silently if staticcheck absent) |

## Known sandbox constraints

- **Single-host flock(2) semantics**: tests that assume cross-process flock
  will work in-sandbox (single linux container is one host). Tests that
  span containers won't.
- **No prebuilt tools committed yet**: `.sandbox/bin/linux-*/` is empty
  for now. `make cross` (run locally) will populate it when we want
  golangci-lint or other heavy tools available offline. Until then,
  setup.sh assumes only `go` and `jq` from the base image.
- **Ephemeral build cache**: `setup.sh` runs `warm_test_cache`, but expect
  cold-compile cost on fresh containers.

## Asking Codex for work

Phase requests:

1. Code change + compile.
2. Targeted tests for the change.
3. (Optional) broader validation.

Prefer module-scoped tasks over "harden the whole repo." For
concurrency/race claims, ask for a minimal in-sandbox reproducer and full
confirmation in CI.

## Beads in this repo

`bd ready` lists actionable issues. The active epic is `loto-7wp` ("Harden
loto for multi-agent production use"). Children are individually titled and
include acceptance criteria — read those before starting work.
