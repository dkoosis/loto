# ADR 0001: integration with `next` (claim+lock flow)

**Status:** accepted  
**Date:** 2026-04-28

## context

`next claim` returns a path with a lease (an advisory work item). `loto try file`
takes an exclusive flock on that path. Today an agent must compose both manually:

```sh
p=$(next claim --treatment=X) && loto try file "$p" && work && loto release && next done --path "$p"
```

Three integration options were considered.

## options

**Option 1 — status quo, documented one-liner.** Each tool stays single-purpose.
Agents compose them in shell. loto knows nothing about next; next knows nothing
about loto. The one-liner above is the full protocol.

**Option 2 — `loto with-next claim` wrapper.** loto gains a subcommand that
calls `next claim`, acquires the flock, execs a user-supplied command, then
releases the lock and calls `next done`. loto's CLI becomes coupled to next's
API surface.

**Option 3 — `next --lock-with=loto` flag.** next gains awareness of loto and
calls it internally. Ownership of the composition lives in next, not loto.

## decision

**Option 1.** Separability is the stronger invariant.

loto's purpose is exclusive coordination, not workflow orchestration. Baking a
`with-next` subcommand into loto couples two tools that evolve on different
cadences. Any bug in the composition logic is a loto bug; any change to next's
claim API breaks loto. Option 3 inverts the coupling without solving it.

The one-liner is Unix-idiomatic, scriptable, and pipeable. Shell functions or
a thin project-local wrapper script are the right abstraction layer if the
composition becomes repetitive — not a new loto subcommand.

The forcing function for Option 2 or 3 is proven operational pain: agents
repeatedly failing to compose the two tools correctly, or a hard requirement
for atomic claim+lock semantics that shell cannot provide. Neither condition
exists today.

## consequences

- No `loto with-next` command. The docs/NORTH_STAR.md protocol one-liner is
  authoritative.
- If repeated pain surfaces, revisit with a concrete failure case.
- loto remains dependency-free with respect to next's API.
