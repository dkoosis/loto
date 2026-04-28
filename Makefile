.PHONY: all build test fmt vet lint tidy install clean

BIN_DIR := bin
BIN     := $(BIN_DIR)/loto
PKG     := ./...

all: fmt vet test build

build:
	@mkdir -p $(BIN_DIR)
	go build -o $(BIN) ./cmd/loto

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
