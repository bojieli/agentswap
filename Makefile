# agentswap has no third-party dependencies, so the Go toolchain is the whole
# toolchain. Everything here is a convenience, not a requirement.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -X main.version=$(VERSION)
GOBIN   ?= $(shell go env GOPATH)/bin

.DEFAULT_GOAL := check

.PHONY: build
build: ## build ./agentswap for this machine
	go build -trimpath -ldflags "$(LDFLAGS)" -o agentswap ./cmd/agentswap

.PHONY: install
install: ## install agentswap into $(GOBIN)
	go install -trimpath -ldflags "$(LDFLAGS)" ./cmd/agentswap

.PHONY: test
test: ## run every test, including the end-to-end suite
	go test -race -shuffle=on ./...

.PHONY: unit
unit: ## run only the fast tests, skipping the subprocess suite
	go test -race -short ./...

.PHONY: e2e
e2e: ## run only the end-to-end suite, which drives the compiled binary
	go test -v ./e2e/

.PHONY: cover
cover: ## merge unit and end-to-end coverage, then open the HTML view
	@rm -rf .coverdata coverage.out
	@mkdir -p .coverdata
	go test -count=1 -cover ./internal/... ./cmd/... -args -test.gocoverdir="$(CURDIR)/.coverdata"
	E2E_COVERDIR="$(CURDIR)/.coverdata" go test -count=1 ./e2e/
	go tool covdata percent -i=.coverdata
	go tool covdata textfmt -i=.coverdata -o=coverage.out
	@go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out

.PHONY: vulncheck
vulncheck: ## check the standard library against the Go vulnerability database
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

.PHONY: fmt
fmt: ## rewrite everything with gofmt
	gofmt -w .

.PHONY: vet
vet:
	go vet ./...

.PHONY: fmtcheck
fmtcheck:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "These files need gofmt:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: lint
lint: ## run golangci-lint, if it is installed
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "golangci-lint not found — see https://golangci-lint.run/welcome/install/"; exit 1; }
	golangci-lint run

.PHONY: deps
deps: ## fail if a third-party dependency has crept in
	@if [ -s go.sum ]; then \
		echo "go.sum is not empty — a third-party dependency was added:"; cat go.sum; exit 1; \
	fi
	@echo "no third-party dependencies"

.PHONY: check
check: build vet fmtcheck deps test ## everything CI runs, minus the linter

# Kept in step with the release workflow's matrix.
CROSS_TARGETS := \
	linux/amd64 linux/arm64 \
	darwin/amd64 darwin/arm64 \
	windows/amd64 windows/arm64 \
	freebsd/amd64

.PHONY: cross
cross: ## compile every release target, to catch a platform-specific break early
	@set -eu; \
	for target in $(CROSS_TARGETS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "==> $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build -o /dev/null ./cmd/agentswap; \
	done

.PHONY: dev
dev: ## run a daemon against a scratch pool, never your real credentials
	AGENTSWAP_HOME=/tmp/agentswap-dev go run ./cmd/agentswap serve -v

.PHONY: release-formula
release-formula: ## generate a Homebrew formula from release archives (TAG=v1.2.3)
	@test -n "$(TAG)" || { echo 'usage: make release-formula TAG=v1.2.3'; exit 2; }
	scripts/generate-homebrew-formula.sh "$(TAG)" "$${DIST:-dist}" "$${OUTPUT:-$${DIST:-dist}/agentswap.rb}"

.PHONY: release-formula-test
release-formula-test: ## smoke-test Homebrew formula generation without a network
	scripts/test-release-formula.sh

.PHONY: clean
clean:
	rm -rf agentswap dist coverage.out .coverdata

.PHONY: help
help: ## list the targets worth knowing about
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  %-10s %s\n", $$1, $$2}'
