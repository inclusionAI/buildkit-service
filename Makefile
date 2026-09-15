BATCH_BINARY  := ./bin/buildctl-batch
BATCH_PKG     := ./cmd/buildctl-batch
DAEMON_BINARY := ./bin/buildctl-daemon
DAEMON_PKG    := ./cmd/buildctl-daemon
BINARIES      := $(BATCH_BINARY) $(DAEMON_BINARY)
GO            ?= go
UV            ?= uv
GOARCH        ?= $(shell $(GO) env GOARCH)
GOOS          ?= linux

.PHONY: all clean test buildctl-batch buildctl-daemon

all: buildctl-batch buildctl-daemon

buildctl-batch:
	mkdir -p $(dir $(BATCH_BINARY))
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(BATCH_BINARY) $(BATCH_PKG)

buildctl-daemon:
	mkdir -p $(dir $(DAEMON_BINARY))
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
		$(GO) build -o $(DAEMON_BINARY) $(DAEMON_PKG)

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
