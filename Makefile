# ekPanel build tasks. Run `make help` for the list.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DATA_DIR ?= .data

.PHONY: help build ui panel agent release test test-go test-web dev dev-ui dev-agent clean

help: ## Show this help
	@grep -hE '^[a-z-]+:.*## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*## "}{printf "  %-12s %s\n", $$1, $$2}'

build: ui panel agent ## Build the web UI and both binaries into bin/

ui: ## Build the web UI and embed it into the panel
	cd web && npm ci --no-audit --no-fund && npm run build
	find internal/webui/dist -mindepth 1 ! -name .keep -delete
	cp -r web/dist/. internal/webui/dist/

panel: ## Build bin/ekpanel
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ekpanel ./cmd/ekpanel

agent: ## Build bin/ekpanel-agent
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ekpanel-agent ./cmd/ekpanel-agent

release: ui ## Build linux amd64/arm64 archives with checksums into dist/
	rm -rf dist && mkdir -p dist
	set -e; for arch in amd64 arm64; do \
		dir=dist/ekpanel-$(VERSION)-linux-$$arch; mkdir -p $$dir; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/ekpanel ./cmd/ekpanel; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(LDFLAGS)' -o $$dir/ekpanel-agent ./cmd/ekpanel-agent; \
		cp deploy/install.sh LICENSE $$dir/; \
		tar -C dist -czf $$dir.tar.gz $$(basename $$dir); rm -rf $$dir; \
	done
	cd dist && sha256sum *.tar.gz > SHA256SUMS && cat SHA256SUMS

test: test-go test-web ## Run all tests and checks

test-go: ## Go tests (starts an embedded PostgreSQL; first run downloads it)
	go vet ./...
	go test -race ./...

test-web: ## Typecheck the UI and check translations
	cd web && npm run typecheck && npm run i18n:check

dev: ## Run the panel on http://127.0.0.1:8080 with an embedded dev database
	go run ./cmd/ekpanel --dev-db --data-dir $(DATA_DIR) --secure-cookies=false --agent-listen 127.0.0.1:7443

dev-ui: ## Run the Vite dev server on http://localhost:5173 (proxies to `make dev`)
	cd web && npm run dev

dev-agent: ## Run an agent here. First time: make dev-agent TOKEN=<token> PIN=<ca pin> (both from the panel)
	@if [ -n "$(TOKEN)" ]; then go run ./cmd/ekpanel-agent enroll --panel 127.0.0.1:7443 --token $(TOKEN) --ca-pin $(PIN) --config $(DATA_DIR)/agent.json; fi
	go run ./cmd/ekpanel-agent run --config $(DATA_DIR)/agent.json

clean: ## Remove build output (keeps the dev database)
	rm -rf bin dist web/dist
	find internal/webui/dist -mindepth 1 ! -name .keep -delete
