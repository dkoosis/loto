# go-sandbox Makefile.cross.mk — includable cross-compile targets
# Include from consuming project: include .sandbox/lib/Makefile.cross.mk
# Requires: .sandbox/project.conf exists
#
# Reads PROJECT_BINS and PREBUILT_TOOLS from project.conf.
# Version pins below are defaults — override in your Makefile before the include.
# Recipes run under make's default /bin/sh (dash on Linux): POSIX sh only, no pipefail.
# ✗ SHELL := bash here — it would leak into every consumer's recipes, and .SHELLFLAGS
# needs GNU make >= 3.82 (macOS ships 3.81).

GOLANGCI_LINT_VER ?= v2.12.2
GO_ARCH_LINT_VER  ?= v1.15.0
GOVULNCHECK_VER   ?= v1.1.4
GOFUMPT_VER       ?= v0.9.2
GOIMPORTS_VER     ?= v0.39.0
MAGE_VER          ?= v1.15.0
BAT_VER           ?= v0.25.0
HYPERFINE_VER     ?= v1.20.0
# Pinned per architecture and checked after download (surmado review of
# dkoosis/ferret#173: the download had no checksum). arm64 stays glibc because
# hyperfine ships no aarch64-unknown-linux-musl release for $(HYPERFINE_VER)
# (or any release to date) — amd64 and arm64 cannot share musl here.
HYPERFINE_SHA256_AMD64 ?= 3285ec7959285288137043dd81dce0dde056227018a8277532d9a364b4f03c2b
HYPERFINE_SHA256_ARM64 ?= 90875cb1db7a1d797c311174d061728361e58fc70e3b62262a00635ac3b1997c
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
	@LOCAL_GO=$$(go env GOVERSION); LOCAL_GO=$${LOCAL_GO#go}; \
	L_MAJ=$${LOCAL_GO%%.*}; L_REST=$${LOCAL_GO#*.}; L_MIN=$${L_REST%%.*}; L_MIN=$${L_MIN%%[!0-9]*}; LOCAL_GO=$$L_MAJ.$$L_MIN; \
	M_VER=$(GOMOD_VER); M_MAJ=$${M_VER%%.*}; M_REST=$${M_VER#*.}; \
	MOD_MIN=$$M_MAJ$$(printf '%03d' $${M_REST%%.*}); \
	LOC_MIN=$$L_MAJ$$(printf '%03d' $$L_MIN); \
	if [ "$$LOC_MIN" -lt "$$MOD_MIN" ]; then \
		echo "FATAL: local go$$LOCAL_GO < go.mod go$(GOMOD_VER)"; \
		exit 1; \
	fi; \
	echo "  local go$$LOCAL_GO >= go.mod go$(GOMOD_VER) — ok"
	@mkdir -p $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)
	@# All tool installs go here; use shell var instead of $(eval) to avoid parse-time trap
	@. .sandbox/project.conf; \
	xtool_build() { \
		tmpmod=$$(mktemp -d) && \
		( cd "$$tmpmod" && go mod init xtool >/dev/null 2>&1 && \
		  go get "$$1@$$2" && \
		  CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) go build -trimpath -ldflags='-s -w' \
		    -o "$(CURDIR)/$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/$$3" "$$1" ); \
		xtool_rc=$$?; \
		rm -rf "$$tmpmod"; \
		return $$xtool_rc; \
	}; \
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
			xtool_build github.com/golangci/golangci-lint/v2/cmd/golangci-lint $(GOLANGCI_LINT_VER) golangci-lint ;; \
		govulncheck) \
			echo "-- govulncheck $(GOVULNCHECK_VER)"; \
			xtool_build golang.org/x/vuln/cmd/govulncheck $(GOVULNCHECK_VER) govulncheck ;; \
		gofumpt) \
			echo "-- gofumpt $(GOFUMPT_VER)"; \
			xtool_build mvdan.cc/gofumpt $(GOFUMPT_VER) gofumpt ;; \
		goimports) \
			echo "-- goimports $(GOIMPORTS_VER)"; \
			xtool_build golang.org/x/tools/cmd/goimports $(GOIMPORTS_VER) goimports ;; \
		snipe) \
			echo "-- snipe"; \
			rm -f $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/snipe; \
			if [ -d "$(SNIPE_SRC)" ]; then \
				echo "  (from $(SNIPE_SRC))"; \
				(cd "$(SNIPE_SRC)" && CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) \
					go build -trimpath -ldflags='-s -w' -o "$(CURDIR)/$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/snipe" .); \
			else \
				xtool_build github.com/dkoosis/snipe latest snipe; \
			fi ;; \
		fo) \
			echo "-- fo"; \
			rm -f $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/fo; \
			if [ -d "$(FO_SRC)" ]; then \
				echo "  (from $(FO_SRC))"; \
				(cd "$(FO_SRC)" && CGO_ENABLED=0 GOOS=linux GOARCH=$(CROSS_ARCH) \
					go build -trimpath -ldflags='-s -w' -o "$(CURDIR)/$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/fo" ./cmd/fo/); \
			else \
				xtool_build github.com/dkoosis/fo/cmd/fo latest fo; \
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
				curl -fsSL "https://github.com/sharkdp/bat/releases/download/$(BAT_VER)/bat-$(BAT_VER)-$$BAT_TRIPLE.tar.gz" -o "$$TMP/a.tgz" && \
					tar xz -C "$$TMP" -f "$$TMP/a.tgz" && \
				cp "$$TMP"/bat-*/bat $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/bat && \
				rm -rf "$$TMP"; \
			fi ;; \
		hyperfine) \
			echo "-- hyperfine $(HYPERFINE_VER)"; \
			if [ -f "$(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/hyperfine" ]; then \
				echo "  (exists, skipping)"; \
			else \
				case "$(CROSS_ARCH)" in \
					amd64) HF_TRIPLE="x86_64-unknown-linux-musl"; HF_SHA256="$(HYPERFINE_SHA256_AMD64)" ;; \
					arm64) HF_TRIPLE="aarch64-unknown-linux-gnu"; HF_SHA256="$(HYPERFINE_SHA256_ARM64)" ;; \
				esac; \
				TMP=$$(mktemp -d); \
				curl -fsSL --connect-timeout 10 --max-time 60 \
					"https://github.com/sharkdp/hyperfine/releases/download/$(HYPERFINE_VER)/hyperfine-$(HYPERFINE_VER)-$$HF_TRIPLE.tar.gz" \
					-o "$$TMP/a.tgz" && \
				if command -v sha256sum >/dev/null 2>&1; then \
					set -- $$(sha256sum "$$TMP/a.tgz"); HF_GOT=$$1; \
				else \
					set -- $$(shasum -a 256 "$$TMP/a.tgz"); HF_GOT=$$1; \
				fi; \
				if [ "$$HF_GOT" != "$$HF_SHA256" ]; then \
					echo "FATAL: hyperfine $(CROSS_ARCH) sha256 mismatch: got $$HF_GOT, want $$HF_SHA256"; \
					rm -rf "$$TMP"; \
					exit 1; \
				fi; \
				tar xz -C "$$TMP" -f "$$TMP/a.tgz" && \
				cp "$$TMP"/hyperfine-*/hyperfine $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/hyperfine && \
				rm -rf "$$TMP"; \
			fi ;; \
		go-arch-lint) \
			echo "-- go-arch-lint $(GO_ARCH_LINT_VER)"; \
			xtool_build github.com/fe3dback/go-arch-lint $(GO_ARCH_LINT_VER) go-arch-lint ;; \
		mage) \
			echo "-- mage $(MAGE_VER)"; \
			xtool_build github.com/magefile/mage $(MAGE_VER) mage ;; \
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
			case "$$f" in *.tmp) rm -f "$$f"; continue;; *.upx) rm -f "$$f"; continue;; esac; \
			case "$$(file "$$f")" in *ELF*) ;; *) continue;; esac; \
			set -- $$(du -h "$$f"); BEFORE=$$1; \
			if upx -t "$$f" >/dev/null 2>&1; then \
				echo "  $$(basename $$f): $$BEFORE (already packed)"; \
				continue; \
			fi; \
			cp "$$f" "$$f.tmp" && \
			upx -q --best --no-backup "$$f.tmp" >/dev/null 2>&1 && \
			case "$$(file "$$f.tmp")" in *ELF*) true;; *) false;; esac && { \
				mv "$$f.tmp" "$$f"; \
				set -- $$(du -h "$$f"); AFTER=$$1; \
				echo "  $$(basename $$f): $$BEFORE -> $$AFTER"; \
			} || { rm -f "$$f.tmp"; echo "  $$(basename $$f): $$BEFORE (skipped — upx failed or produced invalid binary)"; }; \
		done; \
	else \
		echo "-- upx not found, skipping (brew install upx)"; \
	fi
	@echo "-- result:"
	@du -sh $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/
	@ls -lhS $(SANDBOX_BIN_DIR)/linux-$(CROSS_ARCH)/
