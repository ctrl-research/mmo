# Toolchain versions are pinned in .tool-versions. Run `mise install` first.
#
# Code-generation tools live in tools/go.mod so their dependency tree never
# enters the server's build graph.

GO      ?= go
BUFTOOL := $(GO) tool -modfile=tools/go.mod buf

.PHONY: help
help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Build the server binary
	$(GO) build -o bin/mmo ./cmd/mmo

.PHONY: test
test: ## Run all Go tests (database tests skip without --services)
	MMO_TEST_DATABASE_URL="$(DATABASE_URL)" \
	MMO_TEST_REDIS_ADDR="localhost:$(REDIS_PORT)" \
		$(GO) test ./...

.PHONY: test-conformance
test-conformance: wasm ## Verify the WASM build matches the Go build exactly
	cd client && node test/wasm-conformance.mjs

.PHONY: test-all
test-all: test test-conformance ## Run every test, Go and cross-build

.PHONY: test-race
test-race: ## Run all Go tests under the race detector
	MMO_TEST_DATABASE_URL="$(DATABASE_URL)" \
	MMO_TEST_REDIS_ADDR="localhost:$(REDIS_PORT)" \
		$(GO) test -race ./...

.PHONY: lint
lint: ## Vet Go code, lint protobuf, typecheck the client
	$(GO) vet ./...
	$(BUFTOOL) lint
	cd client && npm run typecheck

.PHONY: fmt
fmt: ## Format Go code
	$(GO) fmt ./...

.PHONY: generate
generate: ## Regenerate protobuf types
	$(BUFTOOL) generate

.PHONY: proto-breaking
proto-breaking: ## Check the wire protocol for breaking changes against main
	$(BUFTOOL) breaking --against '.git#branch=main'

.PHONY: golden
golden: ## Regenerate simulation golden fixtures (review the diff carefully)
	$(GO) test ./internal/world/sim -update
	@echo
	@echo "Fixtures regenerated. A diff here means movement changed --"
	@echo "commit it alongside the change that caused it."

.PHONY: wasm
wasm: ## Build the simulation as WebAssembly for client-side prediction
	GOOS=js GOARCH=wasm $(GO) build -o client/public/sim.wasm ./cmd/simwasm
	cp "$$($(GO) env GOROOT)/lib/wasm/wasm_exec.js" client/public/wasm_exec.js
	@# Both land in client/public, which Vite copies into dist on build.

# Port the game listens on. 8080 is occupied on many machines by Docker, Lima,
# and similar, so it is easy to override: make run PORT=8088
PORT ?= 8080

# Local Postgres and Redis. Host ports are overridable because the standard
# ones are often already taken.
POSTGRES_PORT ?= 5433
REDIS_PORT ?= 6379
DATABASE_URL ?= postgres://mmo:devpassword@localhost:$(POSTGRES_PORT)/mmo?sslmode=disable

.PHONY: services
services: ## Start Postgres and Redis
	POSTGRES_PORT=$(POSTGRES_PORT) REDIS_PORT=$(REDIS_PORT) \
		docker compose -f deploy/docker-compose.yml up -d postgres redis

.PHONY: services-down
services-down: ## Stop Postgres and Redis
	docker compose -f deploy/docker-compose.yml down

.PHONY: run
run: build client-build ## Build everything and serve the whole game from one port
	@echo
	@echo "  Open http://localhost:$(PORT)"
	@echo
	DATABASE_URL="$(DATABASE_URL)" \
		./bin/mmo --dev-auth --addr=:$(PORT) --client-dir=client/dist

.PHONY: client-install
client-install: ## Install client dependencies
	cd client && npm ci

.PHONY: client-build
client-build: wasm ## Build the client bundle
	cd client && npm run build

.PHONY: dev
dev: ## Print instructions for the two-process live-reload setup
	@echo "Live reload needs two terminals:"
	@echo
	@echo "  1)  ./bin/mmo --dev-auth --addr=:$(PORT) --log-level=debug"
	@echo "  2)  MMO_SERVER=http://localhost:$(PORT) npm --prefix client run dev"
	@echo
	@echo "Then open the URL Vite prints -- it is usually http://localhost:5173,"
	@echo "but Vite moves to the next free port if that one is taken."
	@echo
	@echo "For a single-process setup with no proxy, use 'make run' instead."

.PHONY: up
up: ## Start the full stack (postgres, redis, server, client)
	docker compose -f deploy/docker-compose.yml up --build

.PHONY: down
down: ## Stop the stack
	docker compose -f deploy/docker-compose.yml down

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin client/dist client/public/sim.wasm
