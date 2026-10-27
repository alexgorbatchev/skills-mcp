set dotenv-load := false

# List available recipes
default:
    @just --list

# Download module dependencies
setup:
    go mod download

# Run the daemon in the foreground
run *args:
    go run ./cmd/skills-mcp server run {{args}}

# Run the CLI with arguments
cli *args:
    go run ./cmd/skills-mcp {{args}}

# Run the CLI in agent-facing mode
run-ai *args:
    AGENT=1 go run ./cmd/skills-mcp {{args}}

# Run the test suite with the race detector
test:
    go test -race ./...

# Fail when any package is below 90% statement coverage
coverage:
    #!/usr/bin/env bash
    set -euo pipefail
    out=$(go test -cover ./...)
    echo "$out"
    echo "$out" | awk '/coverage:/ { gsub("%", "", $5); if ($5 + 0 < 90) { print "below 90%: " $2; bad = 1 } } END { exit bad }'

# Build the binary into bin/
build:
    mkdir -p bin
    go build -ldflags "-X main.version=$(git describe --tags --always --dirty)" -o bin/skills-mcp ./cmd/skills-mcp

# Install the binary into GOBIN
install:
    go install -ldflags "-X main.version=$(git describe --tags --always --dirty)" ./cmd/skills-mcp

# Check module hygiene, formatting, and static analysis
lint:
    go mod tidy -diff
    test -z "$(gofmt -l .)"
    go vet ./...

# Run lint, tests, and the coverage gate
check: lint test coverage

# Format Go code
fmt:
    go fmt ./...

# Remove build output and scratch files
clean:
    rm -rf bin .tmp dist
