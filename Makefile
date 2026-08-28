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
test: ## Run all Go tests
	$(GO) test ./...

.PHONY: test-conformance
test-conformance: wasm ## Verify the WASM build matches the Go build exactly
	cd client && node test/wasm-conformance.mjs

.PHONY: test-all
test-all: test test-conformance ## Run every test, Go and cross-build

.PHONY: test-race
test-race: ## Run all Go tests under the race detector
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

.PHONY: up
up: ## Start the full stack (postgres, redis, server, client)
	docker compose -f deploy/docker-compose.yml up --build

.PHONY: down
down: ## Stop the stack
	docker compose -f deploy/docker-compose.yml down

.PHONY: clean
clean: ## Remove build artefacts
	rm -rf bin client/dist client/public/sim.wasm
