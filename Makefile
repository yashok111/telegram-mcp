BINARY := bin/telegram-mcp
STATE_DIR ?= $(HOME)/.claude/channels/telegram

# `go test -race ./...` and golangci-lint peak multi-GB; on a small box the
# kernel OOM-killer can SIGKILL the surrounding process (e.g. an editor or
# Claude Code session) instead of the build. When a usable `systemd-run --user`
# exists we run those targets in a transient cgroup scope with a memory cap, so
# a blowup is killed inside the scope (the target fails) rather than taking down
# the session. No-op on CI / hosts without a reachable user manager.
# Override per-invocation: make test MEM_MAX=6G MEM_HIGH=5G  (or MEMCAP= to disable)
MEM_MAX  ?= 4G
MEM_HIGH ?= 3G
GO_TEST_PFLAG ?= -p=2
MEMCAP_OK := $(shell command -v systemd-run >/dev/null 2>&1 && systemctl --user show -p Version >/dev/null 2>&1 && echo yes)
ifeq ($(MEMCAP_OK),yes)
MEMCAP := systemd-run --user --scope --quiet --collect -p MemoryMax=$(MEM_MAX) -p MemoryHigh=$(MEM_HIGH) --
endif

.PHONY: build run install clean tidy lint lint-fix test check

build:
	@mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

tidy:
	go mod tidy

lint:
	$(MEMCAP) golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

test:
	$(MEMCAP) go test -race $(GO_TEST_PFLAG) ./...

check: lint test build

run: build
	./$(BINARY)

install: build
	@echo "binary: $(PWD)/$(BINARY)"
	@echo "register with: claude mcp add telegram -s user -- $(PWD)/$(BINARY)"

clean:
	rm -rf bin
