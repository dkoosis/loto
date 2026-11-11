# Loto Makefile
#
# Primary: scan check audit report deploy doctor cross
#   scan   — changed pkgs only (fast inner loop)
#   check  — full repo: vet + lint + arch + test + build + repo-standard
#   audit  — everything: +race +fuzz +dupe +nilcheck +deadcode +omitempty +vuln
# Run `make help` for full target list.

.DEFAULT_GOAL := check

# Strict shell for recipes — but ONLY on GNU Make 3.82+, which is where
# `.SHELLFLAGS` was introduced. macOS ships GNU Make 3.81 (Apple's last GPLv2
# build, frozen in 2006) and ignores this variable outright, while CI runs
# ubuntu-latest with make 4.3. So `-e` and `pipefail` are live in CI and dead
# on dk's Mac (loto-uekt).
#
# ‡ Do not write a recipe that DEPENDS on the flags below. A recipe that pipes
#   says `set -o pipefail;` itself, so it gates identically under both makes;
#   `scripts/makefile_check.sh` (make makefilecheck) fails the build on any
#   recipe line that regresses. The flags stay because they cost nothing and
#   help on a modern make — they are a bonus, never the guarantee.
#
# REPORT_CMD opts out via `set +e;` so it can keep emitting output past
# tool failures.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c

# ── Shared sandbox (go-sandbox) ──
include .sandbox/lib/Makefile.doctor.mk
include .sandbox/lib/Makefile.cross.mk

.PHONY: help scan check audit deploy report report-human \
        vet lint arch test race fuzz demo demo-v vuln dupe nilcheck deadcode omitempty stress perf scriptcheck \
        docscheck makefilecheck standard build install tidy clean hooks

# Pinned gate tools: each runs as `go run <pkg>@$(<TOOL>_VER)` or `npx`, so a
# green gate means the tool ran at this version.
GOARCHLINT_VER := v1.15.0
NILAWAY_VER := v0.0.0-20260918162853-acb8859b9031
GOVULNCHECK_VER := v1.9.0
DEADCODE_VER := v0.52.0
JSCPD_VER := 5.1.2
REPO_STANDARD_VER := v0.4.0
OMITEMPTYSCALAR_VER := v0.4.0

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto

PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.Version=$(VERSION) -X main.GitCommit=$(COMMIT)

# The git hooks this repo delivers. Each name is BOTH a dispatcher
# (.githooks/<name>) and a chain dir (.githooks/hooks.d/<name>/); `make hooks`
# refuses to install if either half is absent.
GIT_HOOKS := pre-commit post-merge pre-push post-checkout prepare-commit-msg reference-transaction

# ── Per-checkout scratch (loto-4ivy) ──
#
# Parallel agent worktrees under .claude/worktrees/ are routine here, and every
# one of them runs this Makefile against the same $HOME. Anything a recipe
# writes to a path shared across checkouts is a race between lanes, so scratch
# lives HERE, under the checkout that produced it. $(CURDIR) is per-worktree by
# construction; .cache/ and .fo/ are already gitignored.
#
# ‡ A new recipe that needs a scratch file puts it under $(CACHE_DIR) or .fo/.
#   A literal /tmp/<fixed-name> in a recipe is the bug this section closes —
#   `make arch` had one, and two lanes checking at once made each other fail.
CACHE_DIR := $(CURDIR)/.cache

# golangci-lint keys its result cache on the package import path, and every
# worktree of this repo shares the module path. Two checkouts with byte-identical
# sources therefore hit the same entry, and a cached diagnostic replays with the
# ABSOLUTE FILENAME of whichever checkout produced it — so a lint in worktree A
# reports findings at ../worktree-B/internal/... long after B was deleted, with
# `failed to parse file` warnings beside them. .golangci.yml's `\.claude/` path
# exclusion cannot catch it: the replayed path is relative and starts `../`.
# One cache per checkout removes the sharing that makes the replay possible.
# Cold cost, measured: ~22s for a full-repo lint, once per new worktree.
export GOLANGCI_LINT_CACHE ?= $(CACHE_DIR)/golangci-lint

# Every fo-rendered check runs through gate.sh so a producer that dies before
# emitting findings reports its own diagnostic instead of "+ no findings"
# (loto-fcbp). The wrapper preserves the producer's exit status, so these
# targets gate exactly as they did before.
GATE := bash scripts/gate.sh

# Report stream — fo dashboard format. `set +e` opts out of the recipe-wide
# -euo pipefail so report MUST run every tool and emit output even if one
# fails. The outer `|| true` on report targets keeps make exit-0 regardless.
# fo's multiplex protocol accepts only format:sarif and format:testjson.
# Text-emitting tools (build/vet/lint) are routed through `fo wrap diag`
# to convert line diagnostics into SARIF before the section delimiter.
REPORT_CMD = set +e; \
	echo '--- tool:build format:sarif ---'; \
	go build ./... 2>&1 | fo wrap diag --tool build --level error; echo; \
	echo '--- tool:vet format:sarif ---'; \
	go vet $(PKG) 2>&1 | fo wrap diag --tool vet --level error; echo; \
	echo '--- tool:lint format:sarif ---'; \
	golangci-lint run --output.sarif.path=/dev/stdout $(PKG) 2>/dev/null; echo; \
	echo '--- tool:test format:testjson ---'; \
	go test -race -json -cover -count=1 $(PKG) 2>&1; echo

## ---------------------------------------------------------------------
## Primary
## ---------------------------------------------------------------------

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nusage: make <target>\n"} \
		/^## [^-]/ { printf "\n%s\n", substr($$0, 4) } \
		/^[a-zA-Z0-9_-]+:.*?## / { printf "  %-18s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

check: vet lint arch test build docscheck scriptcheck makefilecheck standard ## Full repo: vet + lint + arch + test + build + docs + scripts + repo-standard
	@echo "=== check pass ==="

# Dogfood the fleet gate: repo-standard runs at the pinned REPO_STANDARD_VER and
# is the last prerequisite of check, so it grades the tree the rest of the gate
# passed. Bumping the pin is a deliberate PR.
standard: ## Run repo-standard (fleet SDLC checker) against this repo
	go run github.com/dkoosis/repo-standard/cmd/repo-standard@$(REPO_STANDARD_VER)

# Catches README drift against the Makefile/CLI it describes (loto-qo0y):
# a documented `make <target>` that no longer exists, a `# go ...` comment
# whose claimed flags aren't in the target's actual recipe, or a documented
# `loto <cmd>` no longer in `loto --help`. Depends on `build` explicitly —
# under `make -j`, siblings in `check`'s prerequisite list run in parallel,
# so left-to-right order there is not ordering; only an explicit prerequisite
# here guarantees bin/loto exists before docs_check.sh needs it.
docscheck: build ## README's documented commands still match the Makefile/CLI
	@bash scripts/docs_check.sh --loto-bin $(BIN)

scriptcheck: ## Test the build-tooling shell scripts (scripts/*_test.sh)
	@shopt -s nullglob; scripts=(scripts/*_test.sh); \
	if [ $${#scripts[@]} -eq 0 ]; then \
		echo "+ scriptcheck: no scripts/*_test.sh found — nothing to run"; \
		exit 0; \
	fi; \
	rc=0; failed=""; \
	for t in "$${scripts[@]}"; do \
		if ! bash "$$t"; then \
			rc=1; failed="$$failed $$t"; \
		fi; \
	done; \
	if [ $$rc -ne 0 ]; then \
		echo "✗ scriptcheck: failed:$$failed"; \
	fi; \
	exit $$rc

# Guards loto-uekt: a piped recipe line is gated by `pipefail` only on GNU
# Make 3.82+, so one that does not say `set -o pipefail` itself passes on a Mac
# and fails in CI. The script names every offender and the one-line fix.
makefilecheck: ## Every piped recipe line gates the same under make 3.81 and 4.x
	@bash scripts/makefile_check.sh

audit: check race fuzz dupe nilcheck deadcode omitempty vuln demo ## Exhaustive: +race +fuzz +dupe +nilcheck +deadcode +omitempty +vuln +demo
	@echo "=== audit pass ==="

demo: ## Run CLI primitive demos (fo-rendered: triage line, not the full transcript)
	@echo "--- demo (CLI primitive demos) ---"
	@set -o pipefail; go test -json -run Demo -count=1 ./internal/cli | fo --format llm

demo-v: ## Run CLI primitive demos with the full narrated -v transcript
	@go test -v -run Demo -count=1 ./internal/cli

deploy: install ## Build, install, and verify
	@echo "=== deployed ($$(loto version 2>/dev/null || echo unknown)) ==="

report: ## Structured QA output for agents/tools (always exits 0)
	@( $(REPORT_CMD) ) | fo --format llm || true

report-human: ## Same as report, rendered for humans (always exits 0)
	@( $(REPORT_CMD) ) | fo --format human || true

## doctor target provided by .sandbox/lib/Makefile.doctor.mk
## cross / cross-amd64 / cross-arm64 targets provided by .sandbox/lib/Makefile.cross.mk

## ---------------------------------------------------------------------
## Checks
## ---------------------------------------------------------------------

vet: ## Run go vet (fo-rendered)
	@$(GATE) vet diag -- go vet $(PKG)

# ‡ The JSON report is written under this checkout, NOT /tmp (loto-4ivy). It
# used to be /tmp/loto-archcheck.json — one fixed name for every worktree on the
# machine — so two lanes running `make check` at once had one lane's `tee`
# truncating the file the other lane's `jq` was reading. The loser failed with
# "go-arch-lint found warnings" and no warnings, which reads as a real layering
# break and is not one. Measured 2026-09-06 with two concurrent worktrees.
ARCH_JSON := $(CACHE_DIR)/archcheck.json

# go-arch-lint v1.15.0 (CI's pin, .github/workflows/check.yml) shells out to
# `go list` via x/tools' packages loader, which errors ("internal error:
# package \"fmt\" without types") when the `go` on PATH is newer than go.mod's
# directive — true on a dev machine with a newer Go than CI's setup-go
# installs (measured 2026-09-07: local go1.27.0 vs go.mod's go1.26.4; CI is
# unaffected because setup-go installs exactly go.mod's version). Pinning
# GOTOOLCHAIN to go.mod's own version for this one invocation makes the
# child `go list` calls match CI's toolchain without touching the
# go-arch-lint version pin at all (loto-scmj).
arch: ## Enforce layering (.go-arch-lint.yml, go-arch-lint pinned)
	@mkdir -p $(CACHE_DIR)
	@set -o pipefail; GOTOOLCHAIN=go$(GOMOD_VER) go run github.com/fe3dback/go-arch-lint@$(GOARCHLINT_VER) check --json 2>/dev/null | tee $(ARCH_JSON) | fo wrap archlint | fo --format llm
	@jq -e '.Payload.ArchHasWarnings == false' $(ARCH_JSON) >/dev/null || { \
		echo "✗ go-arch-lint found warnings the fo summary above did not render (loto-lu52 — fo's archlint wrapper drops ArchWarningsNotMatched into an empty SARIF results array):"; \
		jq '.Payload | {ArchWarningsNotMatched, ArchWarningsDeps, ArchWarningsDeepScan}' $(ARCH_JSON); \
		exit 1; \
	}

# Single source of truth: .sandbox/project.conf:GOLANGCI_LINT_VERSION — CI
# reads the same key (.github/workflows/check.yml).
#
# loto-46x1: a PATH/pin mismatch used to print a ⚠ naming the drift and then
# run the unpinned binary anyway — findings silently drifted from CI's while
# the gate reported clean. scripts/lint-locked (ported from ferret#160; also
# GOLANGCILINT in repo-standard/mnemd) closes that: on a mismatch it builds
# the PINNED version once (`go run …@vX.Y.Z`, cached under Go's module cache
# outside this mutex) and execs that instead, so the binary that runs always
# matches the pin — no separate install step, no manual chmod.
GOLANGCILINT := bash scripts/lint-locked

lint: ## Run golangci-lint (full)
	@$(GATE) lint sarif -- $(GOLANGCILINT) run --output.sarif.path=/dev/stdout $(PKG)

# -count=1 defeats Go's test cache, so every local `make check` re-ran the whole
# suite — 81s of the ~150s total, paid again for a one-line docs edit, and paid
# once PER LANE when several worktrees check at the same time (loto-4ivy). CI is
# a fresh runner with a cold cache, so the flag buys it nothing but a name for
# what already happens; locally it is the single biggest cost in the gate.
#
# So: the cache is live locally and the cold run is forced wherever CI is set
# (GitHub Actions sets CI=true on every job). Go invalidates on any change to
# the package, its dependencies, or the files and env vars a test reads, so a
# reused PASS is a PASS for these exact bytes. `make race` still runs cold
# unconditionally — it is the pre-merge gate, not the inner loop.
TEST_COUNT ?= $(if $(CI),-count=1,)

test: ## Run tests with coverage (fo-rendered; Go test cache live locally, cold under CI)
	@$(GATE) test testjson -- go test -json $(TEST_COUNT) -cover $(PKG)

# -timeout is per test binary. go test runs packages in parallel, so under
# -race the slowest package (internal/cli, ~175s alone) is CPU-starved and blew
# the old 5m ceiling — a starved package, not a hang. 20m is headroom.
race: ## Run tests with race detector (slow, fo-rendered)
	@echo "--- race (go test -race, up to 20m, silent until done) ---"
	@$(GATE) race testjson -- go test -race -json -timeout=20m -count=1 $(PKG)

stress: ## Concurrent-agent conformance gauntlet (build-tag stress)
	go test -tags=stress -race -run TestStress -count=1 -timeout=2m ./...

perf: ## Wall-clock bounds (build-tag perf; host-dependent, so never in check or CI)
	go test -tags=perf -run 'TestHook_PrePostMedianUnder50msAt300KB|TestHook_EditDecidesUnder10msAt200Holds' -count=1 -v ./internal/cli

vuln: ## Scan for known vulnerabilities (govulncheck, pinned; fo-rendered)
	@echo "--- vuln (govulncheck) ---"
	@$(GATE) vuln sarif -- go run golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER) -format sarif ./...

# jscpd runs at the pinned version (npx fetches it, so this needs the network)
# and never skips.
dupe: ## Detect duplicate code (jscpd, pinned; fo-rendered)
	@echo "--- dupe (jscpd) ---"
	@set -o pipefail; rm -rf .jscpd-tmp; \
		npx --yes jscpd@$(JSCPD_VER) . --silent --reporters json --output .jscpd-tmp >/dev/null 2>&1 || true; \
		fo wrap jscpd <.jscpd-tmp/jscpd-report.json | fo --format llm

nilcheck: ## Run nilaway (pinned; fo-rendered)
	@echo "--- nilcheck (nilaway) ---"
	@$(GATE) nilcheck diag -- go run go.uber.org/nilaway/cmd/nilaway@$(NILAWAY_VER) -include-pkgs=loto -test=false ./...

# Runs each Fuzz function it finds, FUZZTIME apiece.
FUZZTIME ?= 10s
fuzz: ## Fuzz every Fuzz test (FUZZTIME=10s each)
	@set -e; \
	for pkg in $$(go list ./...); do \
		list=$$(go test -list '^Fuzz' $$pkg) || exit $$?; \
		for f in $$(printf '%s\n' "$$list" | sed -n '/^Fuzz/p'); do \
			echo "fuzz $$pkg $$f"; \
			go test -run '^$$' -fuzz "^$$f$$" -fuzztime=$(FUZZTIME) $$pkg; \
		done; \
	done

# deadcode lists each function no path from main reaches, and exits 0 either way,
# so the target fails on any output.
deadcode: ## Fail on any function unreachable from main (deadcode, pinned)
	@out=$$(go run golang.org/x/tools/cmd/deadcode@$(DEADCODE_VER) ./cmd/loto) || { echo "deadcode: could not run"; exit 1; }; \
	if [ -n "$$out" ]; then echo "$$out"; echo "deadcode: the functions above are unreachable; cut them"; exit 1; fi

# omitemptyscalar exits 3 on a ,omitempty tag on a bool (a false lost on write).
omitempty: ## Fail on a bool field tagged omitempty (omitemptyscalar, pinned)
	go run github.com/dkoosis/repo-standard/cmd/omitemptyscalar@$(OMITEMPTYSCALAR_VER) ./...

## ---------------------------------------------------------------------
## Build
## ---------------------------------------------------------------------

build: ## Build loto binary into bin/
	@mkdir -p $(BIN_DIR)
	go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/loto

install: ## Build and install loto to $GOPATH/bin
	go install -ldflags '$(LDFLAGS)' ./cmd/loto

tidy: ## Tidy go.mod
	go mod tidy

clean: ## Remove build artifacts and this checkout's scratch/lint cache
	rm -rf $(BIN_DIR) $(CACHE_DIR)

hooks: ## Point core.hooksPath at the tracked .githooks/ chain-runner. Local-only, per-clone; run once after cloning.
	@# Writes no git aliases: guards are delivered by hooks only.
	@# A foreign core.hooksPath — a global one, or bd's .beads/hooks — is
	@# REPORTED, never overwritten: whatever it routes to is somebody's live
	@# guard, and the chain-runner's whole point is that both can be installed.
	@set -u; \
	missing=""; \
	for h in $(GIT_HOOKS); do \
		if [ ! -x ".githooks/$$h" ]; then missing="$$missing $$h"; fi; \
		if [ ! -d ".githooks/hooks.d/$$h" ]; then missing="$$missing hooks.d/$$h"; fi; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "✗ hooks: missing dispatcher or chain dir:$$missing"; \
		exit 1; \
	fi; \
	inert=""; \
	for e in .githooks/hooks.d/*/*; do \
		case "$${e##*/}" in *.*) continue;; esac; \
		if [ -f "$$e" ] && [ ! -x "$$e" ]; then inert="$$inert $${e#.githooks/hooks.d/}"; fi; \
	done; \
	if [ -n "$$inert" ]; then \
		echo "⚠ hooks: non-executable entry, skipped by the runner:$$inert"; \
		echo '```bash'; \
		echo "chmod +x .githooks/hooks.d/<hook>/<entry>"; \
		echo '```'; \
		exit 1; \
	fi; \
	cur=$$(git config --get core.hooksPath || true); \
	if [ -n "$$cur" ] && [ "$$cur" != ".githooks" ]; then \
		raw=$$(git config --show-origin --get core.hooksPath 2>/dev/null || true); \
		echo "✗ hooks: core.hooksPath is foreign — nothing changed"; \
		echo "ℹ core.hooksPath=$$cur"; \
		echo "ℹ origin=$$raw"; \
		echo "ℹ .githooks is a chain-runner: keep those hooks by copying each into .githooks/hooks.d/<hook>/ as a 50-* entry"; \
		echo '```bash'; \
		echo "cp $$cur/<hook> .githooks/hooks.d/<hook>/50-<name> && chmod +x .githooks/hooks.d/<hook>/50-<name>"; \
		echo "git config --local core.hooksPath .githooks   # this repo only"; \
		echo "git config --unset-all core.hooksPath         # only if that routing is stale"; \
		echo '```'; \
		exit 1; \
	fi; \
	git config --local core.hooksPath .githooks; \
	echo "✓ hooks core.hooksPath=.githooks"; \
	for h in $(GIT_HOOKS); do \
		names=""; \
		for e in .githooks/hooks.d/$$h/*; do \
			if [ -f "$$e" ] && [ -x "$$e" ]; then names="$$names $${e##*/}"; fi; \
		done; \
		if [ -z "$$names" ]; then names=" (none)"; fi; \
		echo "ℹ $$h$$names"; \
	done

## ---------------------------------------------------------------------
## Utilities
## ---------------------------------------------------------------------

scan: ## Vet + lint + test changed packages only (fast inner loop)
	@set -o pipefail; \
	PKGS=$$( { git diff --name-only HEAD -- '*.go' && git ls-files --others --exclude-standard -- '*.go'; } \
		| awk '{ if (sub(/\/[^\/]*$$/, "") == 0 || $$0 == ".") next; print "./" $$0 }' | sort -u) || exit 1; \
	if [ -z "$$PKGS" ]; then \
		echo "no changed Go packages"; \
	else \
		echo "changed packages: $$PKGS"; \
		go vet $$PKGS && \
		golangci-lint run $$PKGS && \
		go test -count=1 -cover $$PKGS && \
		echo "=== scan pass ==="; \
	fi
