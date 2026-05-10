# Plan — loto-ux3.5: `loto hello` (combined reserve + templated announce)

Shape: standard · Profile: craft · Auto-classified

## Goal

One subcommand that atomically (a) reserves a glob and (b) sends a structured,
parseable hello msg to each named sibling. Replaces the two-step prose pattern
in the loto-coordinate skill so the body format stops drifting per render.

## Surface

```
loto hello <glob> --intent <text>
                  [--to handle1,handle2,...]
                  [--ttl <dur>]
                  [--tiebreaker <text>]
                  [--no-tiebreaker]
```

Defaults:
- `--tiebreaker` = `msg+switch>2min`
- `--to` empty → reserve only, no msgs

## Body format (stable, parseable)

```
loto:llm:v1 hello | handle:<self-handle> | glob:<glob> | intent:<intent> | tiebreaker:<tb>
```

Pipe-delimited, fixed field order. With `--no-tiebreaker` the tiebreaker field
is omitted (still pipe-delimited). Sender's handle = currentAgent().Handle (fall
back to ID).

Constraint: glob, intent, tiebreaker must not contain `|` — reject with usage
error (exit 2).

## Behavior

1. Validate input (glob + intent non-empty, no `|` in fields, mutex `--tiebreaker`/`--no-tiebreaker`).
2. Call `l.Reserve(agent, intent, glob, ttl)`. On error → exit.
3. For each `--to` recipient (split on `,`, trim, drop empties):
   - target = glob (matches existing skill prose; mailbox keyed by hashed path)
   - call `l.SendMsgWith(target, Msg{From: agent, To: handle, Body: <template>})`
   - record per-recipient {handle, sent bool, error string}
4. Emit result. Per-sibling msg failure does NOT abort other sends.

## Output

LLM (default when piped):
```
✓ hello glob:<glob> handle:<self>
   reserved
   sent <h1>
   sent <h2>
   failed <h3> reason:<short>
```
- Sort recipient lines deterministically: sent first (alpha by handle), then failed (alpha by handle).
- No-recipient case: only `reserved` line.

JSON:
```json
{
  "reserved": true,
  "glob": "<glob>",
  "intent": "<intent>",
  "agent": "<self-id>",
  "handle": "<self-handle>",
  "to": [
    {"handle": "h1", "sent": true},
    {"handle": "h3", "sent": false, "error": "..."}
  ]
}
```

Exit codes: 0 success (all recipients sent or empty), 1 partial (≥1 failure), 2 usage, 3 system.

## Files

- `cmd/loto/hello.go` (new) — command + body template + emitter dispatch
- `cmd/loto/main.go` — register `helloCmd()` in `init()`
- `internal/render/llm.go` — `EmitLLMHello` + `HelloRecipient` + `HelloResult` types
- `cmd/loto/hello_test.go` (new) — integration tests

## Tests (TDD: red → green → refactor)

1. `TestHello_ReserveOnly` — `--to` empty: reservation present, no msgs written, exit 0.
2. `TestHello_SendsStructuredBody` — single sibling: msg body equals template byte-for-byte.
3. `TestHello_NoTiebreaker` — `--no-tiebreaker`: body omits tiebreaker field.
4. `TestHello_MultiSibling` — two siblings, both sent, deterministic output order.
5. `TestHello_RejectsPipeInIntent` — `--intent "foo|bar"` exits 2.
6. `TestHello_RejectsBothTiebreakerFlags` — `--tiebreaker x --no-tiebreaker` exits 2.

(One-failure-doesn't-abort behavior is library-driven; SendMsgWith is hard to
fail per-recipient in isolation without injecting failures. Cover with a focused
unit test on the dispatch helper if needed; otherwise document as best-effort.)

## Out of scope (defer)

- Updating the loto-coordinate cc-plugins skill to call `loto hello` — bead `loto-0fb` already filed.
- Per-sibling failure injection harness — tracked separately if needed.
- Globs containing `|` (rejected at parse).
