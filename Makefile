.PHONY: help test ui-build e2e-quick e2e-full e2e

help:
	@echo "Targets:"
	@echo "  make test                       # run unit tests"
	@echo "  make ui-build                   # build frontend and sync to internal/ui/static"
	@echo "  make e2e-quick                  # run quick e2e (smoke,compression)"
	@echo "  make e2e-full                   # run full e2e (smoke,compression,orchestrator,semisync)"
	@echo "  make e2e SCENARIOS=a,b,c        # run custom e2e scenarios"

test:
	go test ./...

ui-build:
	./scripts/build-ui.sh

e2e-quick:
	./scripts/e2e/run-suite.sh --profile quick

e2e-full:
	./scripts/e2e/run-suite.sh --profile full

e2e:
	@if [ -z "$(SCENARIOS)" ]; then \
		echo "usage: make e2e SCENARIOS=smoke,compression"; \
		exit 1; \
	fi
	./scripts/e2e/run-suite.sh --scenarios "$(SCENARIOS)"
