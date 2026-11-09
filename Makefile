# ekPanel build tasks. Run `make help` for the list.

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
DATA_DIR ?= .data

.PHONY: help build ui panel agent ekctl release sign test test-go test-web dev dev-ui dev-agent clean

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

ekctl: ## Build bin/ekctl, the command-line tool for the API
	CGO_ENABLED=0 go build -trimpath -ldflags '$(LDFLAGS)' -o bin/ekctl ./cmd/ekctl

# Releases are reproducible: the same commit and VERSION give the same files.
SOURCE_DATE_EPOCH ?= $(shell git log -1 --format=%ct 2>/dev/null || echo 0)
RELEASE_LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)$(if $(RELEASE_EXTRA_KEY), -X ekpanel/internal/release.extraKey=$(RELEASE_EXTRA_KEY))
RELEASE_URL ?= https://github.com/ekPanel/ekpanel/releases

release: ui ## Build a release (VERSION=v1.2.3) into dist/: archives, install.sh, release.json, SHA256SUMS
	@case "$(VERSION)" in v[0-9]*.[0-9]*.[0-9]*) ;; *) echo "VERSION must look like v1.2.3, not $(VERSION)"; exit 1 ;; esac
	rm -rf dist && mkdir -p dist
	sed 's|^EKPANEL_VERSION=.*|EKPANEL_VERSION="$(VERSION)"|' deploy/install.sh > dist/install.sh
	chmod 0755 dist/install.sh
	set -e; for arch in amd64 arm64; do \
		dir=dist/ekpanel-$(VERSION)-linux-$$arch; mkdir -p $$dir; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(RELEASE_LDFLAGS)' -o $$dir/ekpanel ./cmd/ekpanel; \
		CGO_ENABLED=0 GOOS=linux GOARCH=$$arch go build -trimpath -ldflags '$(RELEASE_LDFLAGS)' -o $$dir/ekpanel-agent ./cmd/ekpanel-agent; \
		cp dist/install.sh LICENSE $$dir/; \
		tar --sort=name --mtime=@$(SOURCE_DATE_EPOCH) --owner=0 --group=0 --numeric-owner \
			-C dist -cf - $$(basename $$dir) | gzip -n -9 > $$dir.tar.gz; \
		rm -rf $$dir; \
	done
	set -e; for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do \
		os=$${target%/*}; arch=$${target#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		dir=dist/ekctl-$(VERSION)-$$os-$$arch; mkdir -p $$dir; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags '$(RELEASE_LDFLAGS)' -o $$dir/ekctl$$ext ./cmd/ekctl; \
		cp LICENSE $$dir/; \
		tar --sort=name --mtime=@$(SOURCE_DATE_EPOCH) --owner=0 --group=0 --numeric-owner \
			-C dist -cf - $$(basename $$dir) | gzip -n -9 > $$dir.tar.gz; \
		rm -rf $$dir; \
	done
	printf '{"version":"%s","published_at":"%s","notes_url":"%s/tag/%s"}\n' "$(VERSION)" \
		"$$(date -u -d @$(SOURCE_DATE_EPOCH) +%Y-%m-%dT%H:%M:%SZ)" "$(RELEASE_URL)" "$(VERSION)" > dist/release.json
	cd dist && sha256sum *.tar.gz install.sh release.json > SHA256SUMS && cat SHA256SUMS

sign: ## Sign dist/SHA256SUMS with an Ed25519 key: make sign KEY=path/to/key.pem
	@test -n "$(KEY)" || (echo "KEY=path/to/release-signing.key is required"; exit 1)
	openssl pkeyutl -sign -inkey "$(KEY)" -rawin -in dist/SHA256SUMS -out dist/SHA256SUMS.sig
	openssl pkeyutl -verify -pubin -inkey $(or $(PUBKEY),internal/release/release.pub) -rawin -in dist/SHA256SUMS -sigfile dist/SHA256SUMS.sig

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
