# Triage — loto-cq6

Persistence reviewer pass (1 specialist, adversarial). Findings + decisions.

| # | Finding | Tier | Decision | Reason |
|---|---------|------|----------|--------|
| 1 | dir-fsync correctness (macOS+Linux) | — | clean | `os.File.Sync` → fsync (F_FULLFSYNC on Darwin) on dir fd; durable both platforms |
| 2 | best-effort asymmetry sound? | — | clean | claimSessionCache MUST swallow — returning would discard a won O_EXCL claim on a flush hiccup (caller treats non-ErrExist as fatal). Returning there would be the bug. |
| 3 | ordering (content sync before dir sync) | — | clean | all 3 sites fsync content, then rename/close, then dir |
| 4 | fd leak / double-close in syncDir | — | clean | single close on every path |
| 5 | MkdirAll-created dirs not fsync'd into parent (registry.go:269/492) | P2 | **defer** | outside bead's 3-site scope; net-adds code; tiny window (dirs created once early, reused). → follow-up bead |
| 6 | doctor.go quarantine renames lack parent-dir fsync (308/317/326/331) | P2 | **defer** | recovery-path, not publish hot path; bead names 3 sites only. → follow-up bead |

Applied: 0 (no findings against the diff itself).
Deferred: 2 (folded into one follow-up bead).
Rejected: 0.

D9 held: no "while I'm here" — both deferrals are real signal but outside the
approved three-site scope, so they go to a new bead, not this PR.
