BINARY  := podcast-migrate
MODULE  := github.com/tyler/podcast-migrate
VERSION := $(shell git describe --tags --exact-match 2>/dev/null || git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -ldflags="-X $(MODULE)/cmd.version=$(VERSION)"

.PHONY: build install clean

## build: compile the binary into ./podcast-migrate with the current version baked in
build:
	go build $(LDFLAGS) -o $(BINARY) .

## install: install the binary into $GOPATH/bin with the current version baked in
install:
	go install $(LDFLAGS) .

## clean: remove the compiled binary
clean:
	rm -f $(BINARY)
