#!/usr/bin/env bash
#
# makefile_check.sh — refuse a recipe line that pipes without pipefail.
#
# Why this exists (loto-uekt). The Makefile declares:
#
#     SHELL := /bin/bash
#     .SHELLFLAGS := -euo pipefail -c
#
# `.SHELLFLAGS` arrived in GNU Make 3.82. macOS ships GNU Make 3.81 — Apple's
# last GPLv2 build, frozen in 2006 — which ignores the variable outright. CI
# runs ubuntu-latest (make 4.3+), where it applies. So a recipe that leans on
# `-e` or `pipefail` is enforced on CI and silently unenforced on a Mac, which
# is the worst direction: `make check` is the local signal used to call a PR
# ready, and it is the weaker of the two.
#
# The repo's answer is to not depend on `.SHELLFLAGS` at all: any recipe line
# that pipes says `set -o pipefail;` itself, so it behaves the same under both
# makes. This script is the regression guard for that rule.
#
# A line is exempt when it says `set -o pipefail` / `set -euo pipefail`, when
# it ends in `|| true` (deliberately non-gating), or when it carries the
# marker `# make-strict: ok` with a reason.
#
# Run: make makefilecheck   (or: bash scripts/makefile_check.sh [Makefile])

set -uo pipefail

makefile=${1:-}
if [ -z "$makefile" ]; then
	here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
	makefile=$(cd "$here/.." && pwd)/Makefile
fi

if [ ! -f "$makefile" ]; then
	echo "✗ makefile_check: no such file: $makefile" >&2
	exit 2
fi

# Report paths relative to cwd when we can (design.md: prefer relative).
rel=$makefile
if [ "${makefile#"$PWD"/}" != "$makefile" ]; then
	rel=${makefile#"$PWD"/}
fi

findings=0
declare -a rows=()

# Join continuation lines: make hands one shell the whole backslash-continued
# recipe line, so that joined form is the unit that either has pipefail or
# does not. `start` remembers where the logical line began, for the report.
joined=""
start=0
lineno=0

flush() {
	local text=$1 at=$2
	[ -z "$text" ] && return 0

	# Look for a pipe that make's shell would actually honor. Two things are
	# not that: `||` (a fallback, not a pipe), and a `|` inside a quoted
	# program argument — jq filters and awk scripts are full of them, and the
	# shell never sees those as pipes.
	local probe=$text
	# Double-quoted spans come out FIRST: an apostrophe inside one (`fo's`)
	# would otherwise mis-pair the single-quote pass and expose a `|` that is
	# really inside a jq filter.
	probe=$(printf '%s' "$probe" | sed -e 's/"[^"]*"//g' -e "s/'[^']*'//g")
	# NOT `&&` as the replacement: bash 5.2 reads `&` in a ${//} replacement as
	# the matched text, so `||` would expand to `||||` and every fallback would
	# read as two pipes. A literal placeholder is version-proof.
	probe=${probe//\|\|/_OR_}
	case "$probe" in
	*"|"*) ;;
	*) return 0 ;;
	esac

	case "$text" in
	*"set -o pipefail"* | *"set -euo pipefail"*) return 0 ;;
	*"# make-strict: ok"*) return 0 ;;
	esac

	# A line that ends by discarding its own status is opting out on purpose.
	local trimmed=${text%%[[:space:]]}
	case "$trimmed" in
	*"|| true") return 0 ;;
	esac

	rows+=("✗ $rel:$at pipe-without-pipefail recipe=${text:0:72}")
	findings=$((findings + 1))
}

in_recipe=0

while IFS= read -r line || [ -n "$line" ]; do
	lineno=$((lineno + 1))

	# Track whether a tab-indented line is a RECIPE line or the continuation
	# of a variable assignment. Both are tab-indented; only the first is run
	# by a shell. A rule line opens a recipe block; any other non-tab,
	# non-blank line closes it. (REPORT_CMD's continuations are the case that
	# makes this necessary — they pipe on purpose, under `set +e`.)
	if [ "${line#	}" = "$line" ]; then
		if [ -n "$joined" ]; then
			:
		elif [ -z "${line//[[:space:]]/}" ]; then
			in_recipe=0
			continue
		else
			case "$line" in
			\#*) ;;
			*:=* | *::=* | *+=* | *\?=*) in_recipe=0 ;;
			*=*) in_recipe=0 ;;
			*:*) in_recipe=1 ;;
			*) in_recipe=0 ;;
			esac
			continue
		fi
	fi

	if [ -z "$joined" ] && [ "$in_recipe" -eq 0 ]; then
		continue
	fi

	if [ -z "$joined" ]; then
		start=$lineno
		joined=${line#	}
	else
		joined="$joined ${line#	}"
	fi

	# Still continued? keep accumulating.
	if [ "${joined%\\}" != "$joined" ]; then
		joined=${joined%\\}
		continue
	fi

	flush "$joined" "$start"
	joined=""
done <"$makefile"

flush "$joined" "$start"

if [ "$findings" -eq 0 ]; then
	echo "✓ makefilecheck: 0 recipe lines pipe without pipefail"
	exit 0
fi

echo "✗ makefilecheck count=$findings — a piped recipe line is only gated on GNU Make 3.82+"
printf '%s\n' "${rows[@]}"
cat <<'FIX'

Each line above is enforced on CI (make 4.3) and NOT on macOS (make 3.81),
so it can pass locally and fail in CI. Say pipefail in the recipe itself:

```bash
	@set -o pipefail; producer | renderer
```

Deliberately non-gating? End the line with `|| true`, or annotate it
`# make-strict: ok — <reason>`.
FIX
exit 1
