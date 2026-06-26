.PHONY: dev build test test-verbose lint swagger help

# ── Development ──────────────────────────────────────────────────────────────

dev: ## Start the server with hot-reload (KUBECONFIG from your env or the devcontainer)
	air -c .air.toml

build: ## Compile the binary to bin/server
	go build -o bin/server ./cmd/server

test: ## Run all tests
	go test ./...

test-verbose: ## Run all tests with verbose output
	go test -v ./...

lint: ## Run golangci-lint
	golangci-lint run ./...

swagger: ## Regenerate Swagger docs from annotations
	swag init -g cmd/server/main.go -o docs

# ── Help ──────────────────────────────────────────────────────────────────────

help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.DEFAULT_GOAL := help
