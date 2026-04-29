.PHONY: all build test fmt vet lint tidy install clean check report

# Strict shell for recipes: fail on first error, undefined var, or pipe failure.
SHELL := /bin/bash
.SHELLFLAGS := -euo pipefail -c

# ── Shared sandbox (go-sandbox) ──
include .sandbox/lib/Makefile.doctor.mk
include .sandbox/lib/Makefile.cross.mk

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto
PKG     := ./...
VERSION ?= dev

all: fmt vet lint test build

## check: fast validation — fmt, vet, lint, test, build (sandbox-safe)
check: fmt vet lint test build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags='-X main.Version=$(VERSION)' -o $(BIN) ./cmd/loto

test:
	go test -race $(PKG)

fmt:
	gofmt -s -w .

vet:
	go vet $(PKG)

lint:
	@if ! command -v golangci-lint >/dev/null 2>&1; then \
		echo "golangci-lint not installed; install via .sandbox or 'go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest'"; \
		exit 1; \
	fi
	golangci-lint run $(PKG)

tidy:
	go mod tidy

install: build
	go install ./cmd/loto

clean:
	rm -rf $(BIN_DIR)

## report: structured QA stream for Codex tooling — fenced sections per tool, exits 0 always
report:
	@echo "--- tool:vet  format:text ---"
	@go vet $(PKG) 2>&1 || true
	@echo ""
	@echo "--- tool:lint format:text ---"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run $(PKG) 2>&1 || true; \
	else \
		echo "golangci-lint not installed"; \
	fi
	@echo ""
	@echo "--- tool:test format:testjson ---"
	@go test -race -json $(PKG) 2>&1 || true
	@echo ""
	@echo "--- tool:build format:text ---"
	@go build -o /dev/null ./cmd/loto 2>&1 || true
