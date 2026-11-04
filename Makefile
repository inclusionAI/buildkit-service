BATCH_BINARY  := ./bin/buildctl-batch
BATCH_PKG     := ./cmd/buildctl-batch
DAEMON_BINARY := ./bin/buildctl-daemon
DAEMON_PKG    := ./cmd/buildctl-daemon
OBSERVER_BINARY := ./bin/buildkit-observer
BINARIES      := $(BATCH_BINARY) $(DAEMON_BINARY) $(OBSERVER_BINARY)
GO            ?= go
UV            ?= uv
GOARCH        ?= $(shell $(GO) env GOARCH)
GOOS          ?= linux

.PHONY: all clean test buildctl-batch buildctl-daemon

all: buildctl-batch buildctl-daemon buildkit-observer

buildctl-batch:
	mkdir -p $(dir $(BATCH_BINARY))
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(BATCH_BINARY) $(BATCH_PKG)

buildctl-daemon:
	mkdir -p $(dir $(DAEMON_BINARY))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(DAEMON_BINARY) $(DAEMON_PKG)

buildkit-observer:
	mkdir -p $(dir $(OBSERVER_BINARY))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) $(GO) build -o $(OBSERVER_BINARY) ./cmd/buildkit-observer

test:
	$(GO) test ./...

clean:
	rm -f $(BINARIES)

# Standalone Docker + kind validation. E2E_RUN_DIR selects an existing run.
# Preserve literal path characters; never interpolate paths into shell recipes.
override E2E_RUN_DIR := $(value E2E_RUN_DIR)
export E2E_RUN_DIR
.PHONY: e2e e2e-setup e2e-build e2e-up e2e-test e2e-logs e2e-down e2e-unit

.PHONY: setup
setup:
	$(UV) sync --locked

e2e-setup:
	$(UV) run --locked python scripts/setup-e2e.py

e2e: e2e-setup
	$(UV) run --locked python tests/e2e/run.py all

e2e-build: e2e-setup
	$(UV) run --locked python tests/e2e/run.py build

e2e-up e2e-test e2e-logs e2e-down:
	$(UV) run --locked python tests/e2e/run.py $(patsubst e2e-%,%,$@)

e2e-unit:
	$(UV) run --locked python -m unittest discover -s tests/e2e -p 'test_*.py'

export DEV_RESET_CONFIRM
# Persistent standalone development environment; independent of E2E runs.
.PHONY: dev-setup dev-up dev-rebuild dev-status dev-logs dev-down dev-reset dev-unit
dev-setup: e2e-setup

dev-up dev-rebuild: dev-setup
	$(UV) run --locked python scripts/dev.py $(patsubst dev-%,%,$@)

dev-status dev-logs dev-down dev-reset:
	$(UV) run --locked python scripts/dev.py $(patsubst dev-%,%,$@)

dev-unit:
	$(UV) run --locked python -m unittest discover -s tests/dev -p 'test_*.py'

# Optional, isolated local Prometheus/Grafana/OTLP/Tempo/Loki validation.
export OBS_RUN_DIR
.PHONY: observability observability-build observability-up observability-test observability-status observability-logs observability-down observability-unit
observability: setup e2e-setup observability-unit
	$(UV) run --locked python scripts/observability.py all

observability-build observability-up observability-test observability-status observability-logs observability-down:
	$(UV) run --locked python scripts/observability.py $(patsubst observability-%,%,$@)

observability-unit:
	$(UV) run --locked python -m unittest discover -s tests/observability -p 'test_*.py'
