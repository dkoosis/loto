#!/usr/bin/env bash
#
# docs_check.sh — the documented-commands cheap check (loto-qo0y AC #3).
#
# README drift on Make targets went unnoticed (the `make test` line claimed
# `-race`, which actually lives on the `race` target) because nothing checked
# docs against the tools they describe. This script is that check: it does
# not know what the docs SHOULD say, only whether what they DO say still
# points at something real.
#
# Two independent checks:
#   1. every `make <target>` referenced inside a fenced code block in
#      README.md is a real Makefile target; a trailing `# go ...` comment
#      that claims a literal invocation must have every token present in
#      that target's `make -n` dry-run output.
#   2. every `loto <subcommand>` documented in README.md's "## commands"
#      block is a word `loto --help` still knows about.
#
# usage: docs_check.sh [--readme PATH] [--makefile-dir DIR] [--loto-bin PATH]
#
# Exit 0: nothing wrong (or nothing to check). Exit 1: at least one finding.
# Exit 2: usage/setup error (bad flag, missing README).

set -uo pipefail

readme="README.md"
makefile_dir="."
loto_bin=""

while [ $# -gt 0 ]; do
	case "$1" in
	--readme)
		readme=$2
		shift 2
		;;
	--makefile-dir)
		makefile_dir=$2
		shift 2
		;;
	--loto-bin)
		loto_bin=$2
		shift 2
		;;
	*)
		printf 'docs_check.sh: unknown arg: %s\n' "$1" >&2
		exit 2
		;;
	esac
done

if [ ! -f "$readme" ]; then
	printf 'docs_check.sh: no such file: %s\n' "$readme" >&2
	exit 2
fi

fails=0
fail() {
	printf '✗ %s\n' "$1"
	fails=$((fails + 1))
}
pass() { printf '✓ %s\n' "$1"; }

# ---------------------------------------------------------------- check 1
# `make <target>` references inside any README fenced code block.
in_block=0
checked_targets=0
while IFS= read -r line; do
	case "$line" in
	'```'*)
		in_block=$((1 - in_block))
		continue
		;;
	esac
	[ "$in_block" -eq 1 ] || continue
	case "$line" in
	'make '*) ;;
	*) continue ;;
	esac

	target=$(printf '%s\n' "$line" | awk '{print $2}')
	comment=$(printf '%s\n' "$line" | sed -n 's/^[^#]*#[[:space:]]*//p')
	checked_targets=$((checked_targets + 1))

	if ! grep -qE "^${target}:" "$makefile_dir"/Makefile "$makefile_dir"/.sandbox/lib/*.mk 2>/dev/null; then
		fail "README documents 'make $target' but no such Makefile target exists"
		continue
	fi

	# Only comments that assert a literal invocation ("go test ...") are
	# checked against the recipe — descriptive comments ("install to
	# \$GOPATH/bin") aren't claims about the underlying command.
	case "$comment" in
	'go '*)
		recipe=$(make -C "$makefile_dir" -n "$target" 2>/dev/null)
		missing=""
		for tok in $comment; do
			case "$recipe" in
			*"$tok"*) ;;
			*) missing="$missing $tok" ;;
			esac
		done
		if [ -n "$missing" ]; then
			fail "README's 'make $target' comment claims '$comment' but the recipe is missing:$missing"
		fi
		;;
	esac
done <"$readme"
if [ "$checked_targets" -gt 0 ] && [ "$fails" -eq 0 ]; then
	pass "make targets: $checked_targets referenced in README, all real"
elif [ "$checked_targets" -eq 0 ]; then
	printf 'ℹ no `make <target>` references found in %s\n' "$readme"
fi

# ---------------------------------------------------------------- check 2
# `loto <subcommand>` references inside README's "## commands" block.
if [ -z "$loto_bin" ] && [ -x "$makefile_dir/bin/loto" ]; then
	loto_bin="$makefile_dir/bin/loto"
fi

if [ -z "$loto_bin" ]; then
	printf 'ℹ loto binary not found (pass --loto-bin or run `make build` first) — skipping command check\n'
else
	help_text=$("$loto_bin" --help 2>&1 || true)
	mode=seek
	checked_cmds=0
	cmd_fails=0
	while IFS= read -r line; do
		case "$mode" in
		seek)
			[ "$line" = "## commands" ] && mode=section
			;;
		section)
			case "$line" in
			'```'*) mode=inblock ;;
			'## '*) mode=done ;;
			esac
			;;
		inblock)
			case "$line" in
			'```'*)
				mode=done
				continue
				;;
			esac
			case "$line" in
			'loto '*)
				sub=$(printf '%s\n' "$line" | awk '{print $2}')
				checked_cmds=$((checked_cmds + 1))
				if ! printf '%s\n' "$help_text" | grep -qw -- "$sub"; then
					fail "README documents 'loto $sub' but '$sub' does not appear in 'loto --help'"
					cmd_fails=$((cmd_fails + 1))
				fi
				;;
			esac
			;;
		esac
		[ "$mode" = done ] && break
	done <"$readme"
	if [ "$checked_cmds" -gt 0 ] && [ "$cmd_fails" -eq 0 ]; then
		pass "loto commands: $checked_cmds referenced in README, all present in --help"
	elif [ "$checked_cmds" -eq 0 ]; then
		printf 'ℹ no "## commands" fenced block found in %s\n' "$readme"
	fi
fi

if [ "$fails" -ne 0 ]; then
	printf '\n✗ docs_check.sh: %d finding(s)\n' "$fails"
	exit 1
fi
exit 0
