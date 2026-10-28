.PHONY: help test build build-linux check-linux-compat check-linux-release-archive check-landing-assets release-check release-check-test ui-build release-assets e2e-topology-check e2e-quick e2e-full e2e-observability e2e-scale e2e-session-kill e2e-sigstop-takeover e2e-fault-matrix e2e-rolling-upgrade e2e migrate-up migrate-down migrate-version migrate-force sqlc-generate sqlc-verify

VERSION ?= $(shell git describe --tags --dirty --always 2>/dev/null || echo devel)
BUILD_COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
BUILD_DATE ?= $(shell date -u +"%Y-%m-%dT%H:%M:%SZ")
VERSION_PKG := binlog_server/cmd/binlog-server/cmd
GO_BUILD_LDFLAGS := -X '$(VERSION_PKG).buildVersion=$(VERSION)' -X '$(VERSION_PKG).buildCommit=$(BUILD_COMMIT)' -X '$(VERSION_PKG).buildDate=$(BUILD_DATE)'

help:
	@echo "Targets:"
	@echo "  make test                       # run unit tests"
	@echo "  make build [VERSION=vX.Y.Z]     # build a local bin/binlog-server binary with embedded UI and CGO disabled"
	@echo "  make build-linux [VERSION=vX.Y.Z] # build a Linux amd64 binary with CGO disabled for older glibc hosts"
	@echo "  make check-linux-compat [VERSION=vX.Y.Z] # build Linux amd64 and assert no dynamic glibc dependency"
	@echo "  make check-linux-release-archive VERSION=vX.Y.Z # verify required assets and glibc safety in the Linux amd64 release tar.gz"
	@echo "  make ui-build                   # test+build console -> internal/ui/static (/ui/), frontend -> internal/ui/classic (/ui-classic/), check ESM plus Console markers"
	@echo "  make release-assets VERSION=v0.1.0 # build release archives + checksums for darwin/linux amd64/arm64"
	@echo "  make release-check [VERSION=vX.Y.Z] # before tagging: notes (EN+zh), CHANGELOG, no stale previous version (file:line), release surfaces; then the full config + releaseprep go tests (default v\$$(cat VERSION))"
	@echo "  make release-check-test         # tests of the release-check gate (fake complete and incomplete releases)"
	@echo "  make e2e-topology-check         # validate E2E database topology without Docker"
	@echo "  make e2e-quick                  # run quick e2e (includes smoke on 5.7/8.0/8.4/Percona, smoke-version-restore, smoke-least-privilege-migrate)"
	@echo "  make e2e-full                   # run full e2e (smoke,compression,orchestrator,semisync)"
	@echo "  make e2e-observability          # run observability e2e (smoke-observability with OTLP tracing to a local sink)"
	@echo "  make e2e-scale                  # opt-in 1000-control-task / 100-live-stream scale evidence"
	@echo "  make e2e-session-kill           # SIGSTOP on MySQL 8.0 and MariaDB, default sweep /api/health"
	@echo "  make e2e-sigstop-takeover       # per_node SIGSTOP/SIGCONT worker takeover (ADR 0011 C3)"
	@echo "  make e2e-fault-matrix           # ADR 0011 §4.2 fault-injection matrix (release tier; skips are reported)"
	@echo "  make e2e-rolling-upgrade        # rolling-upgrade fence e2e: mixed-version gate + migrate up/down prechecks (E3)"
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
	CGO_ENABLED=0 go build -ldflags "$(GO_BUILD_LDFLAGS)" -o $${BINARY:-bin/binlog-server} ./cmd/binlog-server

build-linux: ui-build
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(GO_BUILD_LDFLAGS)" -o $${BINARY:-bin/binlog-server-linux-amd64} ./cmd/binlog-server

check-linux-compat:
	@mkdir -p bin
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "$(GO_BUILD_LDFLAGS)" -o $${BINARY:-bin/binlog-server-linux-amd64} ./cmd/binlog-server
	./scripts/check-linux-compat.sh $${BINARY:-bin/binlog-server-linux-amd64}

check-linux-release-archive:
	@if [ -z "$(VERSION)" ]; then \
		echo "usage: make check-linux-release-archive VERSION=v0.1.0"; \
		exit 1; \
	fi
	@archive_path=""; \
	for candidate in \
		"dist/binlog-server_$(VERSION)_linux_amd64.tar.gz" \
		"dist/binlog-server_$(patsubst v%,%,$(VERSION))_linux_amd64.tar.gz" \
		"dist/$(VERSION)/binlog-server_$(VERSION)_linux_amd64.tar.gz" \
		"dist/$(patsubst v%,%,$(VERSION))/binlog-server_$(patsubst v%,%,$(VERSION))_linux_amd64.tar.gz"; do \
		if [ -f "$$candidate" ]; then \
			archive_path="$$candidate"; \
			break; \
		fi; \
	done; \
	if [ -z "$$archive_path" ]; then \
		echo "archive not found for VERSION=$(VERSION) under dist/"; \
		exit 1; \
	fi; \
	./scripts/check-linux-release-archive.sh "$$archive_path"

check-landing-assets:
	./scripts/check-landing-assets.sh

# The release-prep commit (notes, CHANGELOG, VERSION, README pins, landing) is
# what gets tagged; release.yml runs the same check on the tag before building.
# VERSION defaults to git describe above, so read the VERSION file unless
# VERSION was given on the command line.
RELEASE_CHECK_VERSION = $(if $(filter command line,$(origin VERSION)),$(VERSION),v$(shell cat VERSION))

# release-check runs the gate and then the same go tests every PR runs
# (internal/config's TestVersionPinsMatchVERSIONFile plus the gate's own
# tests), so a missing or stale site fails both. release.yml runs the gate
# with -published=gh; here -published=auto falls back to the CHANGELOG
# without gh.
RELEASE_GATE_TESTS = go test -count=1 ./internal/config ./internal/releaseprep

release-check:
	./scripts/check-release-prep.sh "$(RELEASE_CHECK_VERSION)" $(REF)
	$(RELEASE_GATE_TESTS)

release-check-test:
	$(RELEASE_GATE_TESTS)

ui-build:
	./scripts/build-ui.sh

release-assets:
	VERSION="$(VERSION)" BUILD_COMMIT="$(BUILD_COMMIT)" BUILD_DATE="$(BUILD_DATE)" ./scripts/release-assets.sh

e2e-topology-check:
	./scripts/e2e/topology-contract-test.sh

e2e-quick:
	./scripts/e2e/run-suite.sh --profile quick

e2e-session-kill:
	./scripts/e2e/run-suite.sh --scenarios smoke-session-kill

e2e-sigstop-takeover:
	./scripts/e2e/run-suite.sh --scenarios smoke-sigstop-takeover

e2e-fault-matrix:
	./scripts/e2e/run-suite.sh --scenarios smoke-fault-matrix

e2e-rolling-upgrade:
	./scripts/e2e/run-suite.sh --scenarios smoke-rolling-upgrade

e2e-full:
	./scripts/e2e/run-suite.sh --profile full

e2e-observability:
	./scripts/e2e/run-observability.sh

e2e-scale:
	./scripts/e2e/run-suite.sh --scenarios smoke-scale

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
	bash internal/meta/stamp_sqlc_headers.sh

sqlc-verify:
	@$(MAKE) sqlc-generate
	git diff --exit-code

# Coding-standards gate (docs/develop/coding-standards.md). Kept as its own
# block so it does not overlap the release targets above.
.PHONY: lint codehealth codehealth-baseline codehealth-compare codehealth-regen
LINT_BASE ?= $(shell git merge-base origin/main HEAD)

# golangci-lint findings that the tree at LINT_BASE (default: merge base with origin/main) does not have.
# Code that moved or was renamed keeps its findings' identity; see docs/develop/coding-standards.md.
lint:
	go run ./internal/codehealth/cmd/codehealth -base $(LINT_BASE) lint

# Fail unless the tree holds against internal/codehealth/baseline.json (file size, function complexity and length, issue refs, deadcode).
codehealth:
	go run ./internal/codehealth/cmd/codehealth -deadcode check

# Lower internal/codehealth/baseline.json to today's numbers and add entries for split or renamed code;
# refuses to raise anything. CI checks by content that each new entry came from an old one.
codehealth-baseline:
	go run ./internal/codehealth/cmd/codehealth -deadcode update

# What CI's baseline comparison says about this change: run codehealth-baseline, then compare the
# result with the baseline at LINT_BASE (default: merge base with origin/main), paydown included.
# A new file or function over a limit passes only if its content came from a baseline entry.
codehealth-compare: codehealth-baseline
	go run ./internal/codehealth/cmd/codehealth -base $(LINT_BASE) -paydown compare

# Regenerate the files in generated_files and fail if any differs from what is committed.
codehealth-regen:
	go run ./internal/codehealth/cmd/codehealth regen
	git diff --exit-code
