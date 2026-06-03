.PHONY: install build test vet release

MODULE := github.com/taufinity/cli

VERSION    ?= dev
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
BUILD_TIME ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS    := -X 'github.com/taufinity/cli/commands.Version=$(VERSION)' \
              -X 'github.com/taufinity/cli/commands.GitCommit=$(COMMIT)' \
              -X 'github.com/taufinity/cli/commands.BuildTime=$(BUILD_TIME)'

install:
	@mkdir -p $(HOME)/bin
	GOBIN=$(HOME)/bin go install -ldflags "$(LDFLAGS)" ./cmd/taufinity
	@if ! echo "$$PATH" | tr ':' '\n' | grep -qx "$(HOME)/bin"; then \
		SHELL_RC=""; \
		if [ -f "$(HOME)/.zshrc" ]; then SHELL_RC="$(HOME)/.zshrc"; \
		elif [ -f "$(HOME)/.bashrc" ]; then SHELL_RC="$(HOME)/.bashrc"; \
		elif [ -f "$(HOME)/.bash_profile" ]; then SHELL_RC="$(HOME)/.bash_profile"; \
		fi; \
		if [ -n "$$SHELL_RC" ]; then \
			echo '' >> "$$SHELL_RC"; \
			echo '# Added by taufinity installer' >> "$$SHELL_RC"; \
			echo 'export PATH="$$HOME/bin:$$PATH"' >> "$$SHELL_RC"; \
			echo "Added ~/bin to PATH in $$SHELL_RC — restart your shell or run: source $$SHELL_RC"; \
		else \
			echo "Note: add ~/bin to your PATH to use taufinity from anywhere"; \
		fi; \
	fi
	@echo "Installed: $(HOME)/bin/taufinity"

build:
	go build -ldflags "$(LDFLAGS)" -o taufinity ./cmd/taufinity

test:
	go test ./...

vet:
	go vet ./...

# make release          — auto-increment patch (v0.5.0 → v0.6.0)
# make release V=v1.2.0 — explicit version
release:
	@LATEST=$$(git tag --sort=-version:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | head -1); \
	if [ -z "$$V" ]; then \
		MAJOR=$$(echo $$LATEST | cut -d. -f1 | tr -d v); \
		MINOR=$$(echo $$LATEST | cut -d. -f2); \
		PATCH=$$(echo $$LATEST | cut -d. -f3); \
		V="v$$MAJOR.$$MINOR.$$((PATCH+1))"; \
	fi; \
	echo "Releasing $$V (was $$LATEST)"; \
	git tag $$V && git push origin $$V; \
	echo "Warming Go module proxy…"; \
	curl -sf "https://proxy.golang.org/$(MODULE)/@v/$$V.info" -o /dev/null \
		&& echo "Proxy cached $$V" \
		|| echo "Proxy fetch failed (will catch up within a few minutes)"; \
	echo "Done. Users can now run: taufinity update"
