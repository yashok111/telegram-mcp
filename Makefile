BINARY := bin/telegram-mcp
STATE_DIR ?= $(HOME)/.claude/channels/telegram

.PHONY: build run install clean tidy lint lint-fix test check

build:
	@mkdir -p bin
	go build -trimpath -ldflags="-s -w" -o $(BINARY) ./cmd/server

tidy:
	go mod tidy

lint:
	golangci-lint run ./...

lint-fix:
	golangci-lint run --fix ./...

test:
	go test -race ./...

check: lint test build

run: build
	./$(BINARY)

install: build
	@echo "binary: $(PWD)/$(BINARY)"
	@echo "register with: claude mcp add telegram -s user -- $(PWD)/$(BINARY)"

clean:
	rm -rf bin
