# Loto Makefile
#
# Primary: scan check audit report deploy doctor cross
#   scan   — changed pkgs only (fast inner loop)
#   check  — full repo: vet + lint + arch + test + build + conform
#   audit  — everything: +race +vuln +dupl +nilcheck
# Run `make help` for full target list.

.DEFAULT_GOAL := check

# Strict shell for recipes: fail on first error, undefined var, or pipe failure.
# REPORT_CMD opts out via `set +e;` so it can keep emitting output past
# tool failures.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c

# ── Shared sandbox (go-sandbox) ──
include .sandbox/lib/Makefile.doctor.mk
include .sandbox/lib/Makefile.cross.mk

.PHONY: help scan check audit deploy report report-human \
        vet lint arch test race demo demo-v vuln dupl nilcheck stress scriptcheck \
        selfcheck build install tidy clean hooks

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.Version=$(VERSION) -X main.GitCommit=$(COMMIT)

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

check: vet lint arch test build scriptcheck selfcheck ## Full repo: vet + lint + arch + test + build + scripts + conform
	@echo "=== check pass ==="

# Dogfood the fleet gate (sd-th5.15): conform is pinned as a go.mod tool
# dependency (go.sum-verified); bumping the pin is a deliberate PR.
selfcheck: ## Run conform (fleet SDLC checker) against this repo
	go tool conform

scriptcheck: ## Test the build-tooling shell scripts (scripts/*_test.sh)
	@bash scripts/gate_test.sh

audit: check race vuln dupl nilcheck demo ## Exhaustive: +race +vuln +dupl +nilcheck +demo
	@echo "=== audit pass ==="

demo: ## Run CLI primitive demos (fo-rendered: triage line, not the full transcript)
	@go test -json -run Demo -count=1 ./internal/cli | fo --format llm

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

arch: ## Enforce layering (.go-arch-lint.yml)
	@if ! command -v go-arch-lint >/dev/null 2>&1; then \
		echo "go-arch-lint not installed; 'go install github.com/fe3dback/go-arch-lint@v1.15.0'"; \
		exit 1; \
	fi
	@go-arch-lint check --json 2>/dev/null | tee /tmp/loto-archcheck.json | fo wrap archlint | fo --format llm
	@jq -e '.Payload.ArchHasWarnings == false' /tmp/loto-archcheck.json >/dev/null || { \
		echo "✗ go-arch-lint found warnings the fo summary above did not render (loto-lu52 — fo's archlint wrapper drops ArchWarningsNotMatched into an empty SARIF results array):"; \
		jq '.Payload | {ArchWarningsNotMatched, ArchWarningsDeps, ArchWarningsDeepScan}' /tmp/loto-archcheck.json; \
		exit 1; \
	}

lint: ## Run golangci-lint (full)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed; source .sandbox/activate.sh or 'go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest'"; \
		exit 1; \
	fi
	@$(GATE) lint sarif -- golangci-lint run --output.sarif.path=/dev/stdout $(PKG)

test: ## Run tests with coverage (fo-rendered)
	@$(GATE) test testjson -- go test -json -count=1 -cover $(PKG)

# -timeout is per test binary. go test runs packages in parallel, so under
# -race the slowest package (internal/cli, ~175s alone) is CPU-starved and blew
# the old 5m ceiling — a starved package, not a hang. 20m is headroom.
race: ## Run tests with race detector (slow, fo-rendered)
	@$(GATE) race testjson -- go test -race -json -timeout=20m -count=1 $(PKG)

stress: ## Concurrent-agent conformance gauntlet (build-tag stress)
	go test -tags=stress -race -run TestStress -count=1 -timeout=2m ./...

vuln: ## Scan for known vulnerabilities (fo-rendered)
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "govulncheck not installed (install: go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
		exit 1; \
	fi
	@$(GATE) vuln sarif -- govulncheck -format sarif ./...

# ‡ The skip and the run live in ONE recipe line. Make gives each line its own
# shell, so an `exit 0` on the detection line ends only that shell — the next
# line then ran the missing tool anyway and died on "command not found". The
# same shape is safe under `exit 1` (arch, lint, vuln): make stops there.
dupl: ## Detect duplicate code (jscpd; fo-rendered; skips if not installed — dev-only)
	@if ! command -v jscpd >/dev/null 2>&1; then \
		echo "+ dupl: jscpd not installed — skipped (npm i -g jscpd)"; \
	else \
		rm -rf .jscpd-tmp; \
		jscpd . --silent --reporters json --output .jscpd-tmp >/dev/null 2>&1 || true; \
		fo wrap jscpd <.jscpd-tmp/jscpd-report.json | fo --format llm; \
	fi

nilcheck: ## Run nilaway (fo-rendered; skips if not installed — dev-only)
	@if ! command -v nilaway >/dev/null 2>&1; then \
		echo "+ nilcheck: nilaway not installed — skipped (go install go.uber.org/nilaway/cmd/nilaway@latest)"; \
	else \
		$(GATE) nilaway diag -- nilaway -include-pkgs=loto -test=false ./...; \
	fi

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

clean: ## Remove build artifacts
	rm -rf $(BIN_DIR)

hooks: ## Route git hooks to the tracked .githooks/ dir (bd integration, ccp-th5.2). Local-only, per-clone; run once after cloning.
	@missing=""; \
	for h in pre-commit post-merge pre-push post-checkout prepare-commit-msg; do \
		if [ ! -x ".githooks/$$h" ]; then missing="$$missing $$h"; fi; \
	done; \
	if [ -n "$$missing" ]; then \
		echo "make hooks: missing or non-executable dispatcher(s):$$missing" >&2; \
		exit 1; \
	fi
	git config core.hooksPath .githooks
	@echo "git hooks enabled (.githooks): pre-commit / post-merge / pre-push / post-checkout / prepare-commit-msg (bd)."
	@bd hooks list 2>/dev/null || true
	@# Tree-move claim (ccp-vx4w): git has no pre-checkout hook, so checkout/
	@# switch/restore are guarded via git aliases instead — `-c alias.<verb>=`
	@# inside `loto guard` strips the alias for its own real-git call, so this
	@# does not recurse. Fires for any session (shell, script, agent), not
	@# just wrap flows, since git itself resolves the alias.
	git config alias.checkout '!f(){ loto guard checkout "$$@"; }; f'
	git config alias.switch   '!f(){ loto guard switch "$$@"; }; f'
	git config alias.restore  '!f(){ loto guard restore "$$@"; }; f'
	@echo "git aliases enabled: checkout / switch / restore route through 'loto guard'."

## ---------------------------------------------------------------------
## Utilities
## ---------------------------------------------------------------------

scan: ## Vet + lint + test changed packages only (fast inner loop)
	@PKGS=$$( { git diff --name-only HEAD -- '*.go'; git ls-files --others --exclude-standard -- '*.go'; } \
		| xargs dirname 2>/dev/null | sort -u | sed 's|^|./|' | grep -v '^\./$$'); \
	if [ -z "$$PKGS" ]; then \
		echo "no changed Go packages"; \
	else \
		echo "changed packages: $$PKGS"; \
		go vet $$PKGS && \
		golangci-lint run $$PKGS && \
		go test -count=1 -cover $$PKGS && \
		echo "=== scan pass ==="; \
	fi
