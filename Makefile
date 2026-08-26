# work9flow developer surface.
#
# Conventions:
#   make build       — compile both binaries into ./bin
#   make test        — go test ./...
#   make run-runtime — start work9flowd on 127.0.0.1:7469
#   make smoke       — start runtime, hit /v1/health, /v1/version, /v1/runs
#   make healthcheck — non-interactive TUI hit (used by make smoke)

GO       ?= go
RUNTIME  ?= http://127.0.0.1:7469
ADDR     ?= 127.0.0.1:7469
BIN_DIR  := bin

.PHONY: all build test vet tidy run-runtime smoke healthcheck clean help

all: build test

help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

build: ##  # compile both binaries into ./bin
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/work9flowd ./cmd/work9flowd
	$(GO) build -o $(BIN_DIR)/work9flow  ./cmd/work9flow

test: ##  # go test ./...
	$(GO) test ./...

vet: ##  # go vet ./...
	$(GO) vet ./...

tidy: ##  # go mod tidy
	$(GO) mod tidy

run-runtime: build ##  # start work9flowd on $(ADDR)
	./$(BIN_DIR)/work9flowd --addr=$(ADDR)

healthcheck: build ##  # non-interactive TUI run
	WORK9FLOW_RUNTIME_ENDPOINT=$(RUNTIME) ./$(BIN_DIR)/work9flow --once

smoke: build ##  # boot runtime, exercise endpoints, shut down
	@./scripts/smoke.sh $(ADDR)

# smoke-full was REMOVED in bead work9flow-8w0 (dsh-A.10e).
# It booted the inline OpenAI-compatible DSH path that no longer
# exists in production. It will be re-added once bead work9flow-7dh
# (assembled real-DSH smoke with minimax) lands — until then there is
# no e2e gate that proves a run reaches DONE. See
# runtime/dsh-bridge/tests/ for unit coverage of the new path.

clean: ##  # remove ./bin
	unlink -f $(BIN_DIR)/work9flowd $(BIN_DIR)/work9flow 2>/dev/null || true
	rmdir $(BIN_DIR) 2>/dev/null || true
