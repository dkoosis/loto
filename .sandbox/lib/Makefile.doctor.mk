# go-sandbox v0.2.0 synced 2026-04-05
# go-sandbox Makefile.doctor.mk — includable doctor target
# Include from consuming project: include .sandbox/lib/Makefile.doctor.mk

.PHONY: doctor

doctor: ## Validate required toolchain
	@echo "=== doctor ==="
	@. .sandbox/project.conf; \
	MISSING=0; \
	echo "Required (base image + prebuilt):"; \
	for tool in $$BASE_IMAGE_TOOLS $$PREBUILT_TOOLS; do \
		if command -v "$$tool" >/dev/null 2>&1; then \
			printf "  ok  %-20s %s\n" "$$tool" "$$(command -v $$tool)"; \
		else \
			printf "  MISSING  %-20s\n" "$$tool"; \
			MISSING=$$((MISSING + 1)); \
		fi; \
	done; \
	echo ""; \
	echo "Optional:"; \
	for tool in $$OPTIONAL_TOOLS; do \
		if command -v "$$tool" >/dev/null 2>&1; then \
			printf "  ok  %-20s %s\n" "$$tool" "$$(command -v $$tool)"; \
		else \
			printf "  skip  %-20s (optional)\n" "$$tool"; \
		fi; \
	done; \
	if [ "$$MISSING" -gt 0 ]; then \
		echo ""; \
		echo "$$MISSING required tool(s) missing"; \
		exit 1; \
	fi; \
	echo "=== doctor pass ==="
