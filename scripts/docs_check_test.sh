#!/usr/bin/env bash
#
# docs_check_test.sh — drive scripts/docs_check.sh with deliberately wrong
# and deliberately right fixtures.
#
# Guards the loto-qo0y AC #3 contract: a `make <target>` or `loto <cmd>`
# reference in README.md that no longer points at something real must fail
# the check, and a correct doc must pass it.
#
# Run: make scriptcheck   (or: bash scripts/docs_check_test.sh)

set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$here/.." && pwd)
checker=$here/docs_check.sh
fails=0
ran=0

tmp=$(mktemp -d) || {
	echo "docs_check_test.sh: mktemp failed" >&2
	exit 2
}
trap 'rm -rf "$tmp"' EXIT

fake_makefile="$tmp/Makefile"
cat >"$fake_makefile" <<'EOF'
test: ## Run tests
	go test -json -count=1 -cover ./...

race: ## Run tests with race detector
	go test -race -json -timeout=20m -count=1 ./...

install: ## Install
	go install ./cmd/loto

count10: ## Run tests count=10
	go test -count=10 ./...
EOF

fake_loto="$tmp/fake-loto"
cat >"$fake_loto" <<'EOF'
#!/usr/bin/env bash
cat <<'HELP'
usage: loto <command> [args]

commands:
  lock     Acquire a lock
  status   Show lock state
  version  Print loto version
HELP
EOF
chmod +x "$fake_loto"

# Counterexample for finding 2 (coordinator review, #287): "lock" is not a
# command here — it only appears inside `check`'s description. A bare word
# search over the whole help text would let a removed `lock` command pass.
fake_loto_no_lock="$tmp/fake-loto-no-lock"
cat >"$fake_loto_no_lock" <<'EOF'
#!/usr/bin/env bash
cat <<'HELP'
usage: loto <command> [args]

commands:
  check    Check targets for lock conflicts
  status   Show lock state
  version  Print loto version
HELP
EOF
chmod +x "$fake_loto_no_lock"

# The tool failing to execute (missing/broken binary) must be a hard usage
# error, never silently read as "found nothing" or "everything is missing".
fake_loto_broken="$tmp/fake-loto-broken"
cat >"$fake_loto_broken" <<'EOF'
#!/usr/bin/env bash
echo "loto: fatal: corrupt binary" >&2
exit 127
EOF
chmod +x "$fake_loto_broken"

mkdir -p "$tmp/.sandbox/lib" # docs_check.sh globs this dir; empty is fine

# expect <name> <want-status> <want-substring> -- <checker args...>
expect() {
	local name=$1 want_status=$2 want=$3
	shift 3
	[ "${1:-}" = "--" ] && shift
	ran=$((ran + 1))

	local got
	got=$("$checker" "$@" 2>&1)
	local status=$?

	if [ "$status" -ne "$want_status" ]; then
		printf '✗ %s: exit=%d want=%d\n' "$name" "$status" "$want_status"
		printf '%s\n' "$got" | sed 's/^/    /'
		fails=$((fails + 1))
		return
	fi
	case "$got" in
	*"$want"*) ;;
	*)
		printf '✗ %s: output missing %s\n' "$name" "$want"
		printf '%s\n' "$got" | sed 's/^/    /'
		fails=$((fails + 1))
		return
		;;
	esac
	printf '✓ %s\n' "$name"
}

readme_ok="$tmp/README.ok.md"
cat >"$readme_ok" <<'EOF'
## development

```sh
make test     # go test -json -count=1 -cover ./...
make race     # go test -race -json -timeout=20m -count=1 ./...
make install  # install to $GOPATH/bin
```

## commands

```sh
loto lock <path>...  # acquire
loto status          # show
loto version         # version
```
EOF

readme_bad_target="$tmp/README.bad-target.md"
cat >"$readme_bad_target" <<'EOF'
## development

```sh
make nonexistent-target  # not real
```
EOF

readme_bad_recipe="$tmp/README.bad-recipe.md"
cat >"$readme_bad_recipe" <<'EOF'
## development

```sh
make test     # go test -race ./...
```
EOF

readme_bad_cmd="$tmp/README.bad-cmd.md"
cat >"$readme_bad_cmd" <<'EOF'
## commands

```sh
loto frobnicate  # does not exist
```
EOF

# Counterexample for finding 1 (coordinator review, #287): `-count=1` is a
# STRING PREFIX of `-count=10`, so a substring match would let this pass
# against a recipe that actually says -count=10 — a flag value changed and
# the check said nothing.
readme_bad_count="$tmp/README.bad-count.md"
cat >"$readme_bad_count" <<'EOF'
## development

```sh
make count10  # go test -count=1 ./...
```
EOF

expect clean-readme-passes 0 'targets: 3 referenced' -- \
	--readme "$readme_ok" --makefile-dir "$tmp" --loto-bin "$fake_loto"

expect unknown-target-fails 1 "no such Makefile target exists" -- \
	--readme "$readme_bad_target" --makefile-dir "$tmp" --loto-bin "$fake_loto"

# The exact bug this check exists to catch: README claims `-race` on a
# target whose recipe does not have it (loto-qo0y item 1).
expect wrong-recipe-comment-fails 1 "recipe is missing" -- \
	--readme "$readme_bad_recipe" --makefile-dir "$tmp" --loto-bin "$fake_loto"

expect unknown-cli-command-fails 1 "is not a command name in 'loto --help'" -- \
	--readme "$readme_bad_cmd" --makefile-dir "$tmp" --loto-bin "$fake_loto"

# Finding 1: a substring match would let `-count=1` pass against a recipe
# that actually says `-count=10` — must fail on the whole-token mismatch.
expect stale-flag-value-fails 1 "recipe is missing: -count=1" -- \
	--readme "$readme_bad_count" --makefile-dir "$tmp" --loto-bin "$fake_loto"

# Finding 2: "lock" only appears inside `check`'s description here, not as
# a command name — a bare word search would wrongly pass this.
expect removed-command-fails-not-description-match 1 "is not a command name in 'loto --help'" -- \
	--readme "$readme_ok" --makefile-dir "$tmp" --loto-bin "$fake_loto_no_lock"

# The loto binary failing to execute is a setup error (exit 2), never read
# as "0 findings" or "every command missing" (Codex's note re: her own
# go-arch-lint `||` guard — a tool erroring is not the same fact as a tool
# running clean, or running and finding nothing).
expect broken_loto_bin_is_usage_error 2 "failed to run" -- \
	--readme "$readme_ok" --makefile-dir "$tmp" --loto-bin "$fake_loto_broken"

expect missing_readme_is_usage_error 2 "no such file" -- \
	--readme "$tmp/does-not-exist.md" --makefile-dir "$tmp" --loto-bin "$fake_loto"

# Finding 4: a value-taking flag with nothing after it must be a clean
# usage error (exit 2), not a raw `set -u` unbound-variable crash.
expect flag-missing-value-is-usage-error 2 "requires a value" -- \
	--readme

# The real README.md against the real repo, end to end (build must have run
# first — `make check` orders docscheck after build).
if [ -x "$repo_root/bin/loto" ]; then
	expect real-readme-passes 0 'referenced in README, all real' -- \
		--readme "$repo_root/README.md" --makefile-dir "$repo_root" --loto-bin "$repo_root/bin/loto"
else
	printf 'ℹ real-readme-passes: skipped — %s/bin/loto not built\n' "$repo_root"
fi

if [ "$fails" -ne 0 ]; then
	printf '\n✗ docs_check_test.sh: %d of %d checks failed\n' "$fails" "$ran"
	exit 1
fi
printf '\n✓ docs_check_test.sh: %d checks passed\n' "$ran"
exit 0
