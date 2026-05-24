# pxe-beacon Makefile

# Allow overriding the go binary (handy when /usr/local/go/bin isn't on PATH).
GO       ?= go
BIN      ?= pxe-beacon
PKG      := ./cmd/pxe-beacon
DIST     := dist
VERSION  := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS  := -s -w -X main.version=$(VERSION)

.PHONY: all build test fmt vet tidy clean cross run run-loopback \
        build-linux-amd64 build-linux-arm64 build-darwin-arm64 help

all: build

# Local build for the host platform.
build:
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

# Run the full test suite (Tier 0 + Tier 1 in-process).
test:
	$(GO) test ./...

fmt:
	gofmt -s -w .

vet:
	$(GO) vet ./...

tidy:
	$(GO) mod tidy

# Cross-compile artifacts per PLAN section 1 / acceptance criteria.
cross: build-linux-amd64 build-linux-arm64 build-darwin-arm64

build-linux-amd64:
	mkdir -p $(DIST)
	GOOS=linux  GOARCH=amd64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-amd64  $(PKG)

build-linux-arm64:
	mkdir -p $(DIST)
	GOOS=linux  GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-linux-arm64  $(PKG)

build-darwin-arm64:
	mkdir -p $(DIST)
	GOOS=darwin GOARCH=arm64 $(GO) build -ldflags "$(LDFLAGS)" -o $(DIST)/$(BIN)-darwin-arm64 $(PKG)

# Convenience: run with sudo on the auto-detected interface.
run: build
	sudo ./$(BIN)

# Convenience: loopback Tier-1 smoke. No real PXE boot, but useful
# during development on machines where you don't want to touch privileged
# DHCP ports system-wide. Requires sudo for udp/67 + udp/4011.
run-loopback: build
	sudo ./$(BIN) -listen 127.0.0.1 -tftp-listen 127.0.0.1:6969 \
	              -advertise-ip 127.0.0.1 -http-port 8080 -loglevel info

clean:
	rm -rf $(BIN) $(DIST) *.out *.test

help:
	@echo "targets:"
	@echo "  make             - host build"
	@echo "  make test        - go test ./..."
	@echo "  make cross       - linux/amd64, linux/arm64, darwin/arm64"
	@echo "  make run         - sudo ./pxe-beacon (auto interface)"
	@echo "  make run-loopback - loopback smoke (sudo, high TFTP port)"
	@echo "  make clean       - remove built artifacts"
