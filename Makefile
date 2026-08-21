VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BINARY  := promptster
DIST    := dist
LDFLAGS := -ldflags="-s -w -X main.version=$(VERSION)"

.PHONY: build install release clean experiment

build:
	go build $(LDFLAGS) -o bin/$(BINARY) .

# experiment builds the internal-fleet task-envelope harness (openspec
# practice-effect-experiment, batch-0 task 0.1). Deliberately its own package and
# its own binary: `build` and `release` above compile the root package alone, so
# nothing in experiment/ can reach a candidate's install.
experiment:
	go build -o bin/$(BINARY)-experiment ./experiment

# experiment-install puts the binary at a STABLE path before hooks are wired to
# it. install-hooks writes the running binary's resolved path into
# settings.json, so installing from a worktree's bin/ would leave Claude Code
# pointing at a path that disappears when the worktree is removed — mid-batch,
# silently, and a silently dead C2 gate reads as perfect non-adherence.
EXPERIMENT_BIN := $(HOME)/.promptster-experiment/bin/$(BINARY)-experiment

experiment-install: experiment
	mkdir -p $(dir $(EXPERIMENT_BIN))
	cp bin/$(BINARY)-experiment $(EXPERIMENT_BIN)
	@echo "installed $(EXPERIMENT_BIN)"
	@echo "next:  $(EXPERIMENT_BIN) init --org <orgId>"
	@echo "then:  $(EXPERIMENT_BIN) install-hooks --write ~/.claude/settings.json"

install: build
	cp bin/$(BINARY) /usr/local/bin/$(BINARY)

# release cross-compiles for all supported platforms.
# Artifacts are named promptster-OS-ARCH (no version) so that
# the install.sh curl-pipe-sh installer can fetch them via
# /releases/latest/download/promptster-${OS}-${ARCH}.
release: $(DIST)
	GOOS=linux  GOARCH=amd64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-linux-amd64  .
	GOOS=linux  GOARCH=arm64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-linux-arm64  .
	GOOS=darwin GOARCH=amd64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-darwin-amd64 .
	GOOS=darwin GOARCH=arm64 go build $(LDFLAGS) -o $(DIST)/$(BINARY)-darwin-arm64 .

$(DIST):
	mkdir -p $(DIST)

clean:
	rm -rf bin dist

# Embed a new promptster-vscode extension build.
#
# The .vsix is embedded rather than downloaded: an assessment must not depend on
# a release host being reachable at the moment a candidate begins. Its checksum
# is pinned in vsix/embedded.go and asserted against the bytes by a test, so the
# recorded checksum is a fact about this commit rather than a claim about it.
#
#   make embed-vsix VSIX=../promptster-vscode/dist-vsix/promptster-0.3.0.vsix
#
# Build that artifact with promptster-vscode/scripts/build-vsix.sh, which is
# reproducible — check out the tag, rebuild, and you get the same bytes.
.PHONY: embed-vsix
embed-vsix:
	@test -n "$(VSIX)" || (echo "usage: make embed-vsix VSIX=<path to .vsix>" >&2; exit 1)
	@test -f "$(VSIX)" || (echo "no such file: $(VSIX)" >&2; exit 1)
	@version=$$(basename "$(VSIX)" .vsix | sed 's/^promptster-//'); \
	sha=$$(shasum -a 256 "$(VSIX)" | cut -d' ' -f1); \
	old=$$(ls vsix/*.vsix 2>/dev/null || true); \
	rm -f $$old; \
	cp "$(VSIX)" "vsix/promptster-$$version.vsix"; \
	sed -i.bak -E \
	  -e "s|//go:embed promptster-.*\.vsix|//go:embed promptster-$$version.vsix|" \
	  -e "s|Version = \"[^\"]*\"|Version = \"$$version\"|" \
	  -e "s|SourceTag = \"[^\"]*\"|SourceTag = \"v$$version\"|" \
	  -e "s|SHA256 = \"[^\"]*\"|SHA256 = \"$$sha\"|" \
	  vsix/embedded.go; \
	rm -f vsix/embedded.go.bak; \
	echo "embedded promptster-$$version.vsix"; \
	echo "  sha256 $$sha"
	@go test ./vsix/
