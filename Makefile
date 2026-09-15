.DEFAULT_GOAL := help
SHELL := /bin/sh

.PHONY: help up down logs build test test-integration lint sqlc loadtest clean reset

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

up: ## Start the full stack (postgres, redis, api, worker)
	docker compose up --build -d
	@echo "api on http://localhost:8080 - first run pulls ~2GB of language images"

down: ## Stop the stack, keeping data
	docker compose down

reset: ## Stop the stack and delete all data (needed after a schema change)
	docker compose down -v

logs: ## Tail worker and api logs
	docker compose logs -f api worker

build: ## Compile both binaries locally
	go build -o bin/api ./cmd/api
	go build -o bin/worker ./cmd/worker

test: ## Run unit tests (no Docker required)
	go test -race ./...

test-integration: ## Run sandbox tests against a real Docker daemon
	go test -tags=integration -timeout 20m -v ./internal/executor/...

lint: ## Vet and check formatting
	go vet ./...
	@test -z "$$(gofmt -l . | grep -v '^internal/db/')" || \
		(echo "unformatted files:"; gofmt -l . | grep -v '^internal/db/'; exit 1)

sqlc: ## Regenerate internal/db from db/query and db/migrations
	sqlc generate

loadtest: ## Benchmark a running instance (override N, C, LANG)
	go run ./scripts/loadtest -n $(or $(N),100) -c $(or $(C),10) -lang $(or $(LANG),python)

clean: ## Remove build output
	rm -rf bin
