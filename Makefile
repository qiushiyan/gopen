PREFIX ?= $(HOME)/.local
BIN := $(PREFIX)/bin/gopen

.PHONY: build install check test
build:
	go build -o gopen ./cmd/gopen

# Built beside the destination and renamed into place, so tmux and shells
# resolving gopen through PATH never see a partially written executable.
install:
	@mkdir -p "$(PREFIX)/bin"
	@tmp=$$(mktemp "$(BIN).XXXXXX"); trap 'rm -f "$$tmp"' EXIT; \
	go build -o "$$tmp" ./cmd/gopen && chmod 755 "$$tmp" && mv -f "$$tmp" "$(BIN)"

test:
	go test -race ./...

check: test
	go vet ./...
	@test -z "$$(gofmt -l cmd internal)"
