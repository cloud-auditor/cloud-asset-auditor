# justfile for cloud-asset-auditor
#
# Run `just` (no args) to see the full list of recipes.

set shell := ["bash", "-uc"]

# Build metadata captured once when just loads. Fallbacks let `just build`
# work outside a git checkout (e.g., a tarball extracted in CI).
VERSION := `git describe --tags --always --dirty 2>/dev/null || echo dev`
COMMIT  := `git rev-parse --short HEAD 2>/dev/null || echo none`
DATE    := `date -u +%Y-%m-%dT%H:%M:%SZ`

ldflags := "-s -w" \
    + " -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Version=" + VERSION \
    + " -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Commit=" + COMMIT \
    + " -X github.com/cloud-auditor/cloud-asset-auditor/internal/version.Date="   + DATE

# Default: list recipes.
default:
    @just --list

# Build the auditor binary into ./bin/auditor.
build:
    mkdir -p bin
    CGO_ENABLED=0 go build -trimpath -ldflags='{{ldflags}}' -o bin/auditor ./cmd/auditor

# Run the full test suite with race detection and coverage.
test:
    go test -race -cover ./...

# Rewrite renderer golden files (use after intentionally changing output).
test-update:
    go test ./internal/output/... -update

# Static analysis (requires golangci-lint on PATH).
lint:
    golangci-lint run

# Run the CLI via `go run`. Pass arguments after `--` so just doesn't try to
# parse them — e.g.  just run -- audit --provider none -o json
run *ARGS:
    go run ./cmd/auditor {{ARGS}}

# Build the multi-stage container image. Recipe is included in Phase 1 for
# parity with the plan; the actual Dockerfile lands in Phase 6.
docker:
    docker build \
        --build-arg VERSION={{VERSION}} \
        -t cloud-asset-auditor:{{VERSION}} \
        -f deploy/docker/Dockerfile .

# Tidy go.mod / go.sum (generates go.sum on first run).
tidy:
    go mod tidy

# Quick exit-criteria check for Phase 1 — useful in CI smoke jobs.
smoke: build
    test "$(./bin/auditor audit --provider none -o json)" = "[]"
    ./bin/auditor version
