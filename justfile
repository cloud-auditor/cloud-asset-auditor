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

# Build the multi-stage container image. Tags both :{{VERSION}} (immutable)
# and :latest (convenient for local docker-run).
docker:
    docker build \
        --build-arg VERSION={{VERSION}} \
        --build-arg COMMIT={{COMMIT}} \
        --build-arg DATE={{DATE}} \
        -t cloud-asset-auditor:{{VERSION}} \
        -t cloud-asset-auditor:latest \
        -f deploy/docker/Dockerfile .

# Run the built image. Defaults to serve mode on port 8080; override with
# extra args, e.g. `just docker-run audit --provider none -o json`.
docker-run *ARGS:
    docker run --rm -it -p 8080:8080 cloud-asset-auditor:latest {{ARGS}}

# Tidy go.mod / go.sum (generates go.sum on first run).
tidy:
    go mod tidy

# Quick exit-criteria check for Phase 1 — useful in CI smoke jobs.
smoke: build
    test "$(./bin/auditor audit --provider none -o json)" = "[]"
    ./bin/auditor version
