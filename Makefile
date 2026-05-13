# Loto Makefile
#
# Primary: scan check audit report deploy doctor cross
#   scan   — changed pkgs only (fast inner loop)
#   check  — full repo: vet + lint + test + build
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
        vet lint test race vuln dupl nilcheck stress \
        build install tidy clean

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto
PKG     := ./...
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
LDFLAGS := -X main.Version=$(VERSION) -X main.GitCommit=$(COMMIT)

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

check: vet lint arch test ## Full repo: vet + lint + arch + test + build
	@go build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/loto
	@echo "=== check pass ==="

audit: check race vuln dupl nilcheck ## Exhaustive: +race +vuln +dupl +nilcheck
	@echo "=== audit pass ==="

deploy: install ## Build, install, and verify
	@echo "=== deployed ($$(loto --version 2>/dev/null || echo unknown)) ==="

report: ## Structured QA output for agents/tools (always exits 0)
	@( $(REPORT_CMD) ) | fo --format llm || true

report-human: ## Same as report, rendered for humans (always exits 0)
	@( $(REPORT_CMD) ) | fo --format human || true

## doctor target provided by .sandbox/lib/Makefile.doctor.mk
## cross / cross-amd64 / cross-arm64 targets provided by .sandbox/lib/Makefile.cross.mk

## ---------------------------------------------------------------------
## Checks
## ---------------------------------------------------------------------

vet: ## Run go vet
	go vet $(PKG)

arch: ## Enforce layering (.go-arch-lint.yml)
	@if ! command -v go-arch-lint >/dev/null 2>&1; then \
		echo "go-arch-lint not installed; 'go install github.com/fe3dback/go-arch-lint/v3@latest'"; \
		exit 1; \
	fi
	@go-arch-lint check

lint: ## Run golangci-lint (full)
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed; source .sandbox/activate.sh or 'go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest'"; \
		exit 1; \
	fi
	golangci-lint run $(PKG)

test: ## Run tests with coverage
	go test -count=1 -cover $(PKG)

race: ## Run tests with race detector (slow)
	go test -race -timeout=5m -count=1 $(PKG)

stress: ## Concurrent-agent conformance gauntlet (build-tag stress)
	go test -tags=stress -race -run TestStress -count=1 -timeout=2m ./...

vuln: ## Scan for known vulnerabilities
	@if ! command -v govulncheck >/dev/null 2>&1; then \
		echo "govulncheck not installed (install: go install golang.org/x/vuln/cmd/govulncheck@latest)"; \
		exit 1; \
	fi
	govulncheck ./...

dupl: ## Detect duplicate code (jscpd)
	@if ! command -v jscpd >/dev/null 2>&1; then \
		echo "jscpd not installed — skipping (install: npm i -g jscpd)"; \
		exit 0; \
	fi
	jscpd .

nilcheck: ## Run nilaway (skips if not installed)
	@if ! command -v nilaway >/dev/null 2>&1; then \
		echo "nilcheck: nilaway not installed — skipping (install: go install go.uber.org/nilaway/cmd/nilaway@latest)"; \
		exit 0; \
	fi
	@nilaway -include-pkgs="github.com/dkoosis/loto" ./... 2>&1 || true

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
