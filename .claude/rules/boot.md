# Boot
updated: 2026-05-16

→ enter worktree `loto-gp3/`, execute plan task 1.

1. `cd /Users/vcto/projects/loto/loto-gp3 && cat docs/superpowers/plans/2026-05-15-loto-gp3-store-isp-split.md | head -40`
2. `loto lock` the task-1 files before editing.

✓ done
- rejected loto-81o 3-pkg split; filed loto-gp3 (ISP + locks.go split), plan committed + branch pushed
- closed 81o wontfix-as-specified

‡ traps
- new files can't be `loto lock`'d (lstat fails); stub then lock — see plan task 7.
