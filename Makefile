.PHONY: help test build build-linux ui-build release-assets e2e-quick e2e-full e2e-observability e2e migrate-up migrate-down migrate-version migrate-force sqlc-generate sqlc-verify

help:
	@echo "Targets:"
	@echo "  make test                       # run unit tests"
	@echo "  make build                      # build a local bin/binlog-server binary with embedded UI"
	@echo "  make build-linux                # build a local bin/binlog-server-linux-amd64 binary with embedded UI"
	@echo "  make ui-build                   # build frontend and sync to internal/ui/static"
	@echo "  make release-assets VERSION=v0.1.0 # build release archives + checksums for darwin/linux amd64/arm64"
	@echo "  make e2e-quick                  # run quick e2e (smoke,compression)"
	@echo "  make e2e-full                   # run full e2e (smoke,compression,orchestrator,semisync)"
	@echo "  make e2e-observability          # run observability e2e (smoke-observability)"
	@echo "  make e2e SCENARIOS=a,b,c        # run custom e2e scenarios"
	@echo "  make sqlc-generate              # generate typed SQL code from sqlc.yaml"
	@echo "  make sqlc-verify                # regenerate and check for no git diff"
	@echo "  make migrate-up META_DSN=...    # apply DB migrations"
	@echo "  make migrate-down META_DSN=...  # rollback one migration (blocked when MIGRATE_ENV=prod)"
	@echo "  make migrate-version META_DSN=... # show current migration version"
	@echo "  make migrate-force META_DSN=... VERSION=N # force migration version (blocked when MIGRATE_ENV=prod)"

test:
	go test ./...

build: ui-build
	@mkdir -p bin
	go build -o $${BINARY:-bin/binlog-server} ./cmd/binlog-server

build-linux: ui-build
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o $${BINARY:-bin/binlog-server-linux-amd64} ./cmd/binlog-server

ui-build:
	./scripts/build-ui.sh

release-assets:
	VERSION="$(VERSION)" ./scripts/release-assets.sh

e2e-quick:
	./scripts/e2e/run-suite.sh --profile quick

e2e-full:
	./scripts/e2e/run-suite.sh --profile full

e2e-observability:
	./scripts/e2e/run-suite.sh --scenarios smoke-observability

e2e:
	@if [ -z "$(SCENARIOS)" ]; then \
		echo "usage: make e2e SCENARIOS=smoke,compression"; \
		exit 1; \
	fi
	./scripts/e2e/run-suite.sh --scenarios "$(SCENARIOS)"

migrate-up:
	META_DSN="$(META_DSN)" MIGRATE_ENV="$(MIGRATE_ENV)" go run ./cmd/migrate up

migrate-down:
	META_DSN="$(META_DSN)" MIGRATE_ENV="$(MIGRATE_ENV)" ALLOW_DESTRUCTIVE_MIGRATE="$(ALLOW_DESTRUCTIVE_MIGRATE)" go run ./cmd/migrate down --steps 1

migrate-version:
	META_DSN="$(META_DSN)" MIGRATE_ENV="$(MIGRATE_ENV)" go run ./cmd/migrate version

migrate-force:
	@if [ -z "$(VERSION)" ]; then \
		echo "usage: make migrate-force META_DSN=... VERSION=1"; \
		exit 1; \
	fi
	META_DSN="$(META_DSN)" MIGRATE_ENV="$(MIGRATE_ENV)" ALLOW_DESTRUCTIVE_MIGRATE="$(ALLOW_DESTRUCTIVE_MIGRATE)" go run ./cmd/migrate force "$(VERSION)"

sqlc-generate:
	@GOPROXY=$${GOPROXY:-https://proxy.golang.org,direct} \
	GOSUMDB=$${GOSUMDB:-sum.golang.org} \
	CGO_ENABLED=0 \
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@v1.28.0 generate -f sqlc.yaml

sqlc-verify:
	@$(MAKE) sqlc-generate
	git diff --exit-code
