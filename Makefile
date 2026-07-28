.PHONY: help build test test-coverage-check lint nilcheck sg gitnexus eventlog-gate check-pubkey-sync check-schema-sync govulncheck fix setup ci clean release pre-release changelog changelog-preview

# git exports these into every hook's environment so the hook's own git
# invocations resolve to the repo/worktree that triggered it. If a recipe
# below (or a script it shells out to, e.g. go-crap-gate.sh's own `git diff`)
# inherits a leaked value, an explicit `git -C <dir>` gets silently
# overridden and targets the wrong repo instead of the one it names.
unexport GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_PREFIX GIT_COMMON_DIR GIT_OBJECT_DIRECTORY GIT_ALTERNATE_OBJECT_DIRECTORIES

COVERAGE_THRESHOLD ?= 75
BINARY_NAME=argus
GOBIN=./bin

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo "none")
BUILD_DATE ?= $(shell date -u '+%Y-%m-%d %H:%M:%S UTC')
VERSION_PKG := github.com/Elysium-Labs-EU/argus/internal/buildinfo
LDFLAGS := -ldflags "-X '$(VERSION_PKG).Version=$(VERSION)' -X '$(VERSION_PKG).GitCommit=$(COMMIT)' -X '$(VERSION_PKG).BuildDate=$(BUILD_DATE)' -w -s"

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-28s\033[0m %s\n", $$1, $$2}' | sort

build: ## Build binary
	@mkdir -p $(GOBIN)
	CGO_ENABLED=0 go build $(LDFLAGS) -o $(GOBIN)/$(BINARY_NAME) .

test: ## Run tests
	go test ./... -race -count=2

lint-skill: ## Check skills/argus/SKILL.md's flags/defaults/forge-list against the real CLI (already part of `make test`)
	go test ./cmd/... -run TestSkillMD -v

test-coverage-check: ## Fail if total coverage is below COVERAGE_THRESHOLD
	@go test -coverprofile=coverage.out ./... -covermode=atomic -count=1 2>&1 | grep -v "^?" || true
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/{gsub(/%/,""); print $$3}'); \
	echo "Total coverage: $${total}%"; \
	awk -v total="$${total}" -v threshold="$(COVERAGE_THRESHOLD)" \
		'BEGIN { if (total+0 < threshold+0) { print "Coverage " total "% below threshold " threshold "%"; exit 1 } }'

lint: ## Run all linters
	@command -v golangci-lint >/dev/null 2>&1 || { echo "golangci-lint not found. Run: make setup"; exit 1; }
	golangci-lint run --timeout=5m

nilcheck: ## Static nil-pointer safety analysis
	@command -v nilaway >/dev/null 2>&1 || { echo "nilaway not found. Run: make setup"; exit 1; }
	nilaway ./...

sg: ## Scan codebase with ast-grep rules (skipped until rules/ ported)
	@if [ -d rules ]; then command -v ast-grep >/dev/null 2>&1 || { echo "ast-grep not found. Install: brew install ast-grep"; exit 1; }; ast-grep scan; else echo "no rules/ dir yet, skipping"; fi

gitnexus: ## Index this repo with GitNexus for AI-assisted code search (no install needed, runs via npx)
	npx gitnexus analyze

eventlog-gate: ## Fail if any _test.go file calls eventlog.Open directly instead of eventlog.OpenForTest
	bash scripts/check-eventlog-open.sh

check-pubkey-sync: ## Fail if the release-signing pubkey differs between cmd/update.go and scripts/install.sh
	bash scripts/check-pubkey-sync.sh .

check-schema-sync: ## Fail if schemas/config.schema.json's keys drift from internal/repoconfig/yaml.go's
	bash scripts/check-schema-sync.sh .

govulncheck: ## Reachability-aware vulnerability scan (complements OSV-Scanner's lockfile-only scan)
	@command -v govulncheck >/dev/null 2>&1 || { echo "govulncheck not found. Run: make setup"; exit 1; }
	govulncheck ./...

crap: test-coverage-check ## go-crap change-risk gate on changed functions only (vs origin/main)
	@command -v go-crap >/dev/null 2>&1 || { echo "go-crap not found. Run: make setup"; exit 1; }
	bash scripts/go-crap-gate.sh .

crap-report: ## Full whole-repo go-crap debt report (informational, no gate)
	@command -v go-crap >/dev/null 2>&1 || { echo "go-crap not found. Run: make setup"; exit 1; }
	go-crap scan . --exclude '.*_test\.go'

fix: ## Fix go formatting and struct field alignment
	golangci-lint fmt
	go tool fieldalignment -fix ./...

setup: ## Install dev tools (golangci-lint, nilaway, go-crap, ast-grep) — same versions as eos/themis
	@echo "Installing golangci-lint v2.11.0..."
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(shell go env GOPATH)/bin v2.11.0
	@echo "Installing nilaway..."
	go install go.uber.org/nilaway/cmd/nilaway@latest
	@echo "Installing go-crap (change-risk analysis)..."
	go install github.com/padiazg/go-crap@latest
	@echo "Installing govulncheck..."
	go install golang.org/x/vuln/cmd/govulncheck@latest
	@command -v ast-grep >/dev/null 2>&1 || echo "ast-grep not found — install with: brew install ast-grep (or see https://ast-grep.github.io/guide/quick-start.html)"
	@echo "Setup complete."

ci: test lint sg nilcheck test-coverage-check crap eventlog-gate check-pubkey-sync check-schema-sync govulncheck ## Run all CI checks locally
	@echo "All CI checks passed!"

release: ## Update changelog, tag and push a release (requires TAG=v1.2.0)
	@if [ -z "$(TAG)" ]; then echo "Usage: make release TAG=v1.2.0"; exit 1; fi
	@command -v git-cliff >/dev/null 2>&1 || { echo "git-cliff not found. Install: https://git-cliff.org/docs/installation"; exit 1; }
	git cliff --tag $(TAG) --output CHANGELOG.md
	git add CHANGELOG.md
	git diff --cached --quiet CHANGELOG.md || git commit -m "chore: update changelog for $(TAG)"
	git push origin HEAD
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)

pre-release: ## Tag and push a pre-release (requires TAG=v1.2.0-rc.1, no changelog update)
	@if [ -z "$(TAG)" ]; then echo "Usage: make pre-release TAG=v1.2.0-rc.1"; exit 1; fi
	git tag -a $(TAG) -m "Pre-release $(TAG)"
	git push origin $(TAG)

changelog: ## Generate CHANGELOG.md from git history
	@command -v git-cliff >/dev/null 2>&1 || { echo "git-cliff not found. Install: https://git-cliff.org/docs/installation"; exit 1; }
	git cliff --output CHANGELOG.md

changelog-preview: ## Preview unreleased changes (does not write to file)
	@command -v git-cliff >/dev/null 2>&1 || { echo "git-cliff not found. Install: https://git-cliff.org/docs/installation"; exit 1; }
	git cliff --unreleased

clean: ## Remove build artifacts
	rm -rf $(GOBIN) dist/ coverage.out
	go clean
