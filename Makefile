.PHONY: install build test vet

VERSION    ?= dev
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X 'github.com/taufinity/cli/commands.Version=$(VERSION)' \
              -X 'github.com/taufinity/cli/commands.GitCommit=$(COMMIT)' \
              -X 'github.com/taufinity/cli/commands.BuildTime=$(BUILD_TIME)'

install:
	go install -ldflags "$(LDFLAGS)" ./cmd/taufinity

build:
	go build -ldflags "$(LDFLAGS)" -o taufinity ./cmd/taufinity

test:
	go test ./...

vet:
	go vet ./...
