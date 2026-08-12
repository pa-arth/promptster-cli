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
