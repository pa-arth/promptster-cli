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
