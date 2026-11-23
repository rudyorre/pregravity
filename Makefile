GO ?= $(shell command -v go 2>/dev/null || echo $(HOME)/.local/bin/go)
BIN_DIR ?= bin
BINARY := $(BIN_DIR)/pregravity
INSTALL_DIR ?= $(HOME)/.local/bin
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)

.PHONY: all build test vet fmt install clean

all: vet test build

build:
	@mkdir -p $(BIN_DIR)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/pregravity

test:
	CGO_ENABLED=0 $(GO) test -count=1 ./...

vet:
	CGO_ENABLED=0 $(GO) vet ./...

fmt:
	$(GO) fmt ./...

install: build
	@mkdir -p $(INSTALL_DIR)
	install -m 0755 $(BINARY) $(INSTALL_DIR)/pregravity

clean:
	rm -rf $(BIN_DIR)
