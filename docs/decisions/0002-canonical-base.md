# ADR 0002: canonical coordination base directory

**Status:** accepted  
**Date:** 2026-04-28

## context

Today's default base is `./.loto` (per-tree). Two Claude sessions in sibling
worktrees of the same repo cannot see each other's locks because they each
write to a different `.loto/` directory. This is the single biggest barrier to
multi-agent coordination.

Three options were evaluated:

| Option | Path | Trade-offs |
|--------|------|-----------|
| A | `$XDG_STATE_HOME/loto/projects/<slug>/` | Standards-compliant, hidden, cross-tree |
| B | `~/.loto/projects/<slug>/` | Discoverable, matches trixi convention |
| C | Per-tree with `--shared-base` opt-in | Zero-impact for single-agent use; requires opt-in |

## decision

**Option A:** `$XDG_STATE_HOME/loto/projects/<slug>/`

`$XDG_STATE_HOME` defaults to `~/.local/state` when unset. Project state is
scoped under `projects/<slug>/` so multiple projects on the same host don't
pollute each other.

Agent identity lives separately at `~/.loto/agents/<uuid>.json` (host-global,
per ADR 0001 / NS identity amendment) — not under `projects/<slug>/`.

## slug derivation

The slug identifies the logical project across all its worktrees. Rules (first
that applies):

1. **Stable file pin:** if `$GIT_COMMON_DIR/.loto-slug` exists, read it.
   This is the authoritative source and survives remote rewrites.
2. **git remote origin:** `git remote get-url origin` → strip scheme + host →
   normalize (`/` → `-`, `.git` suffix dropped). Example:
   `github.com/dkoosis/loto` → `dkoosis-loto`.
3. **No remote / multiple remotes:** use `origin` if available; otherwise use
   the last path component of `git rev-parse --show-toplevel`.
4. **No git at all:** use the last path component of `$PWD`.

On first use, the derived slug is written to `$GIT_COMMON_DIR/.loto-slug`
(if writeable) to pin it against future remote changes.

## failure-mode enumeration

| Scenario | Behavior |
|----------|----------|
| (a) No git at all | Fallback to last component of `$PWD`. Warning printed once to stderr. |
| (b) Git but no remote | Fallback to last component of `git rev-parse --show-toplevel`. Warning printed once. |
| (c) Multiple remotes | Use `origin` if present; else first remote alphabetically. Warn if non-`origin` chosen. |
| (d) Origin URL rewritten mid-project | `.loto-slug` pin file takes precedence; no slug drift. Manual `loto reslug` command (future) for intentional moves. |

## migration

No existing users; `./.loto` is not migrated. If `./.loto` is detected in the
current working directory at startup, loto prints a one-time warning to stderr:

```
loto: warning: found legacy ./.loto directory — coordination now uses
$XDG_STATE_HOME/loto/projects/<slug>/. The old directory is not read.
```

The warning is suppressed if `LOTO_SUPPRESS_LEGACY_WARNING=1`.

## consequences

- Sibling worktrees of the same project now share a coordination directory.
- `loto status` from any worktree shows the full project lock picture.
- `defaultBase()` in `cmd/loto/main.go` changes; `LOTO_BASE` override remains.
- `~/.loto/agents/` coexists at the parent level (host-global identity).
