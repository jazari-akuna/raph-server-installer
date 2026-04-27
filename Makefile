# raph-server-installer — local developer tasks.
# Wave 3B (Parcel 3B). Tabs for recipes (mandatory).

REPO_ROOT := $(shell pwd)
TESTS_DIR := $(REPO_ROOT)/tests
ENROL_DIR := $(REPO_ROOT)/stacks/enrol

.PHONY: help test test-keep test-shell test-verbose lint lint-shell lint-go render-check

help: ## Show this help.
	@awk 'BEGIN { FS = ":.*##"; printf "Targets:\n" } \
	      /^[a-zA-Z_-]+:.*##/ { printf "  %-16s %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

test: ## Build the test image and run the harness (≤5 min).
	@bash $(TESTS_DIR)/run-tests.sh

test-verbose: ## Same as `test` but stream container output live.
	@TESTS_VERBOSE=1 bash $(TESTS_DIR)/run-tests.sh

test-keep: ## Run tests but keep the container around for poking.
	@TESTS_KEEP=1 bash $(TESTS_DIR)/run-tests.sh

test-shell: ## Drop into an interactive shell in a freshly-built test container.
	@docker build -t raph-server-installer-tests:dev $(TESTS_DIR)
	@docker run --rm -it \
	  -v $(REPO_ROOT):/opt/raph-server-installer-src:ro \
	  -e TEST_MODE=1 \
	  -e TEST_REPO_SRC=/opt/raph-server-installer-src \
	  raph-server-installer-tests:dev shell

lint: lint-shell lint-go ## Run all linters.

lint-shell: ## Run shellcheck on every *.sh in the repo (tests/ included).
	@set -e; \
	mapfile -t scripts < <(find $(REPO_ROOT) -type f -name '*.sh' \
	  -not -path '*/.git/*' \
	  -not -path '*/.last-run/*' | sort); \
	if [ $${#scripts[@]} -eq 0 ]; then \
	  echo "no shell scripts found"; exit 0; \
	fi; \
	printf '%s\n' "$${scripts[@]}"; \
	shellcheck -x -e SC1091 "$${scripts[@]}"

lint-go: ## Run `go vet` on the enrol Go module.
	@cd $(ENROL_DIR) && go vet ./...

render-check: ## Run scripts/render-templates.sh --check against a baked sample env.
	@bash $(REPO_ROOT)/scripts/render-templates.sh --check
