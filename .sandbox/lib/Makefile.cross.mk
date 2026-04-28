# go-sandbox v0.2.0 synced 2026-04-05
# go-sandbox Makefile.cross.mk — includable cross-compile targets
# Include from consuming project: include .sandbox/lib/Makefile.cross.mk
# Requires: .sandbox/project.conf exists
#
# Reads PROJECT_BINS and PREBUILT_TOOLS from project.conf.
# Version pins below are defaults — override in your Makefile before the include.

GOLANGCI_LINT_VER ?= v2.11.4
GO_ARCH_LINT_VER  ?= v1.14.0
GOVULNCHECK_VER   ?= v1.1.4
GOFUMPT_VER       ?= v0.9.2
GOIMPORTS_VER     ?= v0.39.0
MAGE_VER          ?= v1.15.0
BAT_VER           ?= v0.25.0
SNIPE_SRC         ?= $(HOME)/Projects/snipe
FO_SRC            ?= $(HOME)/Projects/fo
GOMOD_VER         := $(shell awk '/^go /{print $$2}' go.mod)
SANDBOX_BIN_DIR   := .sandbox/bin

.PHONY: cross cross-amd64 cross-arm64

cross: cross-amd64 ## Cross-compile sandbox tools (default: amd64)

cross-amd64: ## Cross-compile linux/amd64 sandbox tools
	@echo "=== cross: linux/amd64 ==="
	@$(MAKE) --no-print-directory _cross-build CROSS_ARCH=amd64

cross-arm64: ## Cross-compile linux/arm64 sandbox tools
	@echo "=== cross: linux/arm64 ==="
	@$(MAKE) --no-print-directory _cross-build CROSS_ARCH=arm64

_cross-build:
	@# Pre-flight: local Go must be >= go.mod target
	@LOCAL_GO=$$(go version | sed 's/.*go\([0-9]*\.[0-9]*\).*/\1/'); \
	MOD_MIN=$$(echo $(GOMOD_VER) | cut -d. -f1)$$(printf '%03d' $$(echo $(GOMOD_VER) | cut -d. -f2)); \
	LOC_MIN=$$(echo $$LOCAL_GO | cut -d. -f1)$$(printf '%03d' $$(echo $$LOCAL_GO | cut -d. -f2)); \
	if [ "$$LOC_MIN" -lt "$$MOD_MIN" ]; then \
		echo "FATAL: local go$$LOCAL_GO < go.mod go$(GOMOD_VER)"; \
		exit 1; \
	fi; \
	echo "  local go$$LOCAL_GO >= go.mod go$(GOMOD_VER) — ok"
	@mkdir -p $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)
	@# All tool installs go here; use shell var instead of $(eval) to avoid parse-time trap
	@. .sandbox/project.conf; \
	XBIN="$$(go env GOPATH)/bin/linux_$(CROSS_ARCH)"; \
	for entry in $$PROJECT_BINS; do \
		name=$${entry%%:*}; path=$${entry#*:}; \
		echo "-- $$name"; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go build -trimpath \
			-ldflags='-s -w -X main.Version=$(VERSION)' \
			-o $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/$$name $$path; \
	done; \
	for tool in $$PREBUILT_TOOLS; do \
		case "$$tool" in \
		golangci-lint) \
			echo "-- golangci-lint $(GOLANGCI_LINT_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VER); \
			cp $$XBIN/golangci-lint $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		govulncheck) \
			echo "-- govulncheck $(GOVULNCHECK_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				golang.org/x/vuln/cmd/govulncheck@$(GOVULNCHECK_VER); \
			cp $$XBIN/govulncheck $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		gofumpt) \
			echo "-- gofumpt $(GOFUMPT_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				mvdan.cc/gofumpt@$(GOFUMPT_VER); \
			cp $$XBIN/gofumpt $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		goimports) \
			echo "-- goimports $(GOIMPORTS_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				golang.org/x/tools/cmd/goimports@$(GOIMPORTS_VER); \
			cp $$XBIN/goimports $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		snipe) \
			echo "-- snipe"; \
			rm -f $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/snipe; \
			if [ -d "$(SNIPE_SRC)" ]; then \
				echo "  (from $(SNIPE_SRC))"; \
				(cd "$(SNIPE_SRC)" && CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) \
					go build -trimpath -ldflags='-s -w' -o "$(CURDIR)/$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/snipe" .); \
			else \
				CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
					github.com/dkoosis/snipe@latest && \
					cp $$XBIN/snipe $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/; \
			fi ;; \
		fo) \
			echo "-- fo"; \
			rm -f $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/fo; \
			if [ -d "$(FO_SRC)" ]; then \
				echo "  (from $(FO_SRC))"; \
				(cd "$(FO_SRC)" && CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) \
					go build -trimpath -ldflags='-s -w' -o "$(CURDIR)/$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/fo" ./cmd/fo/); \
			else \
				CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
					github.com/dkoosis/fo/cmd/fo@latest && \
					cp $$XBIN/fo $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/; \
			fi ;; \
		bat) \
			echo "-- bat $(BAT_VER)"; \
			if [ -f "$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/bat" ]; then \
				echo "  (exists, skipping)"; \
			else \
				case "$(CROSS_ARCH)" in \
					amd64) BAT_TRIPLE="x86_64-unknown-linux-musl" ;; \
					arm64) BAT_TRIPLE="aarch64-unknown-linux-gnu" ;; \
				esac; \
				TMP=$$(mktemp -d); \
				curl -fsSL "https://github.com/sharkdp/bat/releases/download/$(BAT_VER)/bat-$(BAT_VER)-$$BAT_TRIPLE.tar.gz" \
					| tar xz -C "$$TMP" && \
				cp "$$TMP"/bat-*/bat $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/bat && \
				rm -rf "$$TMP"; \
			fi ;; \
		go-arch-lint) \
			echo "-- go-arch-lint $(GO_ARCH_LINT_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				github.com/fe3dback/go-arch-lint@$(GO_ARCH_LINT_VER); \
			cp $$XBIN/go-arch-lint $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		mage) \
			echo "-- mage $(MAGE_VER)"; \
			CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go install -trimpath -ldflags='-s -w' \
				github.com/magefile/mage@$(MAGE_VER); \
			cp $$XBIN/mage $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/ ;; \
		dtree) \
			echo "-- dtree (manually-managed shell script — no version pin or build-from-source)"; \
			if [ -f ".sandbox/codex/dtree" ]; then \
				cp .sandbox/codex/dtree $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/dtree; \
			elif [ -f ".codex/dtree" ]; then \
				cp .codex/dtree $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/dtree; \
			else \
				echo "  dtree source not found, skipping"; \
			fi ;; \
		*) echo "  WARNING: unknown prebuilt tool: $$tool" ;; \
		esac; \
	done
	@# UPX compress (verify compressed binary runs to catch musl/kernel issues)
	@if command -v upx >/dev/null 2>&1; then \
		echo "-- upx compressing"; \
		for f in $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/*; do \
			[ -f "$$f" ] || continue; \
			case "$$f" in *.tmp|*.upx) rm -f "$$f"; continue;; esac; \
			file "$$f" | grep -q ELF || continue; \
			BEFORE=$$(du -h "$$f" | cut -f1); \
			if upx -t "$$f" >/dev/null 2>&1; then \
				echo "  $$(basename $$f): $$BEFORE (already packed)"; \
				continue; \
			fi; \
			cp "$$f" "$$f.tmp" && \
			upx -q --best --no-backup "$$f.tmp" >/dev/null 2>&1 && \
			file "$$f.tmp" | grep -q ELF && { \
				mv "$$f.tmp" "$$f"; \
				AFTER=$$(du -h "$$f" | cut -f1); \
				echo "  $$(basename $$f): $$BEFORE -> $$AFTER"; \
			} || { rm -f "$$f.tmp"; echo "  $$(basename $$f): $$BEFORE (skipped — upx failed or produced invalid binary)"; }; \
		done; \
	else \
		echo "-- upx not found, skipping (brew install upx)"; \
	fi
	@echo "-- result:"
	@du -sh $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/
	@du -h $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/* | sort -rh
