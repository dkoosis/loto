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
# Exemptions, each scoped the way the shell actually scopes it. `set -o
# pipefail` / `set -euo pipefail` is a shell-wide setting, so it clears the
# whole logical line; so does an explicit `# make-strict: ok` marker. `|| true`
# discards the status of its own pipeline and nothing else, so it is judged per
# `;`-separated segment — a trailing advisory fallback does not excuse an
# earlier gating pipeline on the same line.
#
# A `$$( )` command substitution is judged on its own, because it runs in a
# subshell that inherits pipefail and the usual form buries it in double quotes
# (`out="$$(producer | renderer)"`), where the quote-stripping pass would
# otherwise hide the pipeline.
#
# The scan follows `include` lines, so a recipe in .sandbox/lib/*.mk is held
# to the same rule as one in the root Makefile.
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

findings=0
declare -a rows=()
declare -a files=()

# Walk make's include graph: `include a b`, `-include a b`, `sinclude a b`.
# make resolves a relative include against its cwd, which is the including
# file's directory whenever make runs at the repo root — how this repo runs
# it. A missing `-include` is make's own no-op and a missing `include` is
# make's own fatal error, so neither is ours to report; both are skipped.
collect() {
	local f=$1 dir line word p seen
	for seen in "${files[@]-}"; do
		[ "$seen" = "$f" ] && return 0
	done
	files+=("$f")
	dir=$(dirname "$f")
	while IFS= read -r line || [ -n "$line" ]; do
		case "$line" in
		include\ * | -include\ * | sinclude\ *) ;;
		*) continue ;;
		esac
		line=${line#-}
		line=${line#s}
		line=${line#include}
		for word in $line; do
			case "$word" in
			\#*) break ;;
			*'$'*) continue ;;
			esac
			p=$word
			case "$p" in
			/*) ;;
			*) p=$dir/$p ;;
			esac
			[ -f "$p" ] && collect "$p"
		done
	done <"$f"
}

# real_pipe <text> — 0 when the text carries a pipe make's shell would honor.
# Two things are not that: `||` (a fallback, not a pipe), and a `|` inside a
# quoted program argument — jq filters and awk scripts are full of them, and
# the shell never sees those as pipes.
real_pipe() {
	local probe=$1
	# Double-quoted spans come out FIRST: an apostrophe inside one (`fo's`)
	# would otherwise mis-pair the single-quote pass and expose a `|` that is
	# really inside a jq filter.
	probe=$(printf '%s' "$probe" | sed -e 's/"[^"]*"//g' -e "s/'[^']*'//g")
	# NOT `&&` as the replacement: bash 5.2 reads `&` in a ${//} replacement as
	# the matched text, so `||` would expand to `||||` and every fallback would
	# read as two pipes. A literal placeholder is version-proof.
	probe=${probe//\|\|/_OR_}
	case "$probe" in
	*"|"*) return 0 ;;
	esac
	return 1
}

# subs_split <text> — sets SUBS to every `$$( )` body and SUB_REST to the text
# with those bodies removed.
#
# `$$(` is what a recipe writes for a SHELL command substitution; a single `$(`
# is a make function, expanded by make before any shell runs, so it is not this
# guard's business. Bodies are pulled out before the quote-stripping pass in
# real_pipe, because the common form is `out="$$(producer | renderer)"` — the
# pipeline lives inside double quotes and stripping the span would hide it.
subs_split() {
	local s=$1
	local i=0 n=${#s} c depth body
	SUBS=()
	SUB_REST=""
	while [ "$i" -lt "$n" ]; do
		if [ "${s:i:3}" = '$$(' ]; then
			depth=1
			i=$((i + 3))
			body=""
			while [ "$i" -lt "$n" ] && [ "$depth" -gt 0 ]; do
				c=${s:i:1}
				case "$c" in
				'(') depth=$((depth + 1)) ;;
				')') depth=$((depth - 1)) ;;
				esac
				if [ "$depth" -gt 0 ]; then
					body+=$c
				fi
				i=$((i + 1))
			done
			SUBS+=("$body")
			continue
		fi
		SUB_REST+=${s:i:1}
		i=$((i + 1))
	done
}

# split_segments <text> — print each `;`-separated command segment, one per
# line. Quote state is tracked so a `;` inside a jq program or a quoted message
# does not split one segment into two.
split_segments() {
	local s=$1
	local i=0 n=${#s} q="" c seg=""
	while [ "$i" -lt "$n" ]; do
		c=${s:i:1}
		if [ -z "$q" ]; then
			case "$c" in
			\' | \")
				q=$c
				;;
			\\)
				seg+=$c
				i=$((i + 1))
				c=${s:i:1}
				;;
			\;)
				printf '%s\n' "$seg"
				seg=""
				i=$((i + 1))
				continue
				;;
			esac
		elif [ "$c" = "$q" ]; then
			q=""
		fi
		seg+=$c
		i=$((i + 1))
	done
	printf '%s\n' "$seg"
}

report() {
	rows+=("✗ $rel:$1 pipe-without-pipefail recipe=${2:0:72}")
	findings=$((findings + 1))
}

flush() {
	local text=$1 at=$2
	[ -z "$text" ] && return 0

	# Line-scoped exemptions. `set -o pipefail` is a shell-wide setting: once
	# the recipe says it, it holds for every later command in the same shell,
	# so it clears the whole logical line. So does an explicit marker.
	case "$text" in
	*"set -o pipefail"* | *"set -euo pipefail"*) return 0 ;;
	*"# make-strict: ok"*) return 0 ;;
	esac

	local -a SUBS=()
	local SUB_REST="" body seg trimmed
	subs_split "$text"

	# A command substitution runs in a subshell that inherits pipefail, so a
	# pipe inside one diverges between the two makes exactly like a bare one.
	for body in ${SUBS+"${SUBS[@]}"}; do
		if real_pipe "$body"; then
			report "$at" "$body"
		fi
	done

	# `|| true` discards the status of ITS OWN pipeline and nothing else, so —
	# unlike pipefail — it is judged per `;`-separated segment. A trailing
	# advisory fallback must not excuse an earlier gating pipeline on the same
	# line: `@false | true; echo advisory | cat || true` exits at the first
	# pipeline under make 4.x and runs straight through under 3.81.
	while IFS= read -r seg; do
		real_pipe "$seg" || continue
		trimmed=${seg%"${seg##*[![:space:]]}"}
		case "$trimmed" in
		*"|| true") continue ;;
		esac
		report "$at" "$seg"
	done < <(split_segments "$SUB_REST")
}

# scan <makefile> runs the recipe walk over one file. `rel` is what the rows
# print (design.md: prefer paths relative to cwd).
scan() {
	local makefile=$1 line
	rel=$makefile
	if [ "${makefile#"$PWD"/}" != "$makefile" ]; then
		rel=${makefile#"$PWD"/}
	fi

	# Join continuation lines: make hands one shell the whole backslash-
	# continued recipe line, so that joined form is the unit that either has
	# pipefail or does not. `start` remembers where the logical line began.
	joined=""
	start=0
	lineno=0
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
}

collect "$makefile"
for f in "${files[@]}"; do
	scan "$f"
done

if [ "$findings" -eq 0 ]; then
	echo "✓ makefilecheck: 0 recipe lines pipe without pipefail files=${#files[@]}"
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
