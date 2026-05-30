# Boot
updated: 2026-05-30 #10

→ pick next work: `bd ready` — 7 open bugs from the 2026-05-30 audit (3 P2, 4 P3). All static-analysis findings; verify each reproduces against the real binary before investing (gh-open/bead-state lie about fixed-state here — see memory).

✓ done (this session)
- PR #154 open: restore-audit durability (loto-rmyg, loto-1qed) — both High audit findings. AcquireLocks now releases the tx before the detached restore-audit (no self-contention stall); DoctorRepair restore-audit routed through `appendAuditDetached`. Awaiting dk merge.
- 3 stale GH issues closed (#121/122/123 — already fixed on main, never closed).
- go-bug-audit ran: 9 findings → 9 beads, nug `de8816efddc5`. 2 shipped, 7 open.

‡ open audit beads (verify-then-fix)
- P2: loto-pody (unlock --all no-pin false-success), loto-kwlp (PID-reuse stale), loto-h85e (--restore-orphan-mode TOCTOU)
- P3: loto-j863 (SIGKILL strip-window), loto-ta02 (hardlink TOCTOU), loto-zxjx (`--` escape), loto-ltof (lock-free check) — last two likely WONTFIX/design

‡ traps
- `docs/NORTH_STAR.md` churns from a KG reconcile daemon — auto-published, ✗ commit it.
- `commitTxFn`/`vacuumFn`/`fchmodFn` are the test-injection seams in internal/store — use them, don't add new ones.
