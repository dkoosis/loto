.PHONY: all build test fmt vet lint tidy install clean check report

# ── Shared sandbox (go-sandbox) ──
include .sandbox/lib/Makefile.doctor.mk
include .sandbox/lib/Makefile.cross.mk

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto
PKG     := ./...
VERSION ?= dev

all: fmt vet test build

## check: fast validation — fmt, vet, test, build (sandbox-safe)
check: fmt vet test build

build:
	@mkdir -p $(BIN_DIR)
	go build -ldflags='-X main.Version=$(VERSION)' -o $(BIN) ./cmd/loto

test:
	go test -race $(PKG)

fmt:
	gofmt -s -w .

vet:
	go vet $(PKG)

# Optional; skips silently if staticcheck isn't installed.
lint:
	@if command -v staticcheck >/dev/null 2>&1; then \
		staticcheck $(PKG); \
	else \
		echo "staticcheck not installed; skipping (go install honnef.co/go/tools/cmd/staticcheck@latest)"; \
	fi

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
	@echo "--- tool:test format:testjson ---"
	@go test -race -json $(PKG) 2>&1 || true
	@echo ""
	@echo "--- tool:build format:text ---"
	@go build -o /dev/null ./cmd/loto 2>&1 || true
