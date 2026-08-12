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

# Serve the web UI against the built-in synthetic inventory (no credentials).
demo ADDR="127.0.0.1:8080":
    go run ./cmd/auditor serve --demo --include-raw --addr {{ADDR}}

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

# Lint the Helm chart with helm's built-in linter.
helm-lint:
    helm lint deploy/helm/cloud-asset-auditor
    helm lint deploy/helm/cloud-asset-auditor -f deploy/helm/cloud-asset-auditor/examples/values-cronjob.yaml
    helm lint deploy/helm/cloud-asset-auditor -f deploy/helm/cloud-asset-auditor/examples/values-deployment.yaml

# Render every template in both modes and feed through kubectl --dry-run
# for schema validation. Catches issues helm lint alone misses.
helm-template:
    helm template auditor deploy/helm/cloud-asset-auditor -f deploy/helm/cloud-asset-auditor/examples/values-cronjob.yaml    | kubectl apply --dry-run=client -f -
    helm template auditor deploy/helm/cloud-asset-auditor -f deploy/helm/cloud-asset-auditor/examples/values-deployment.yaml | kubectl apply --dry-run=client -f -

# Quick exit-criteria check for Phase 1 — useful in CI smoke jobs.
smoke: build
    test "$(./bin/auditor audit --provider none -o json)" = "[]"
    ./bin/auditor version

# ---------------------------------------------------------------------------
# Frontend (Next.js static export → embedded into the binary)
# ---------------------------------------------------------------------------

# The export is COMMITTED to internal/server/webui/, so `go build`, CI,
# goreleaser, and the Docker image never need Node. Run this after changing
# anything under web/, and commit the regenerated tree with the source change.
#
# --omit=optional skips `sharp`, next's image-optimization engine: this is a
# static export with images.unoptimized, so sharp is never invoked, and it
# carries a native libvips with open CVEs.
#
# Rebuild the embedded web UI from web/ into internal/server/webui/.
web:
    npm --prefix web ci --omit=optional
    npm --prefix web run build
    rm -rf internal/server/webui
    mkdir -p internal/server/webui
    cp -R web/out/. internal/server/webui/
    @echo "embedded web UI refreshed — commit internal/server/webui/"

# Next's dev server proxies nothing, so start the Go API first:
#   just build && ./bin/auditor serve   # terminal 1
#   just web-dev                        # terminal 2 → http://localhost:3000
#
# Serve the frontend with hot reload against a running `auditor serve`.
web-dev:
    npm --prefix web install --omit=optional
    npm --prefix web run dev

# Typecheck the frontend without producing a build.
web-check:
    npm --prefix web run typecheck

# CI runs this so a frontend change that forgets `just web` cannot merge.
#
# Fail if internal/server/webui/ is stale relative to web/.
web-verify:
    #!/usr/bin/env bash
    set -euo pipefail
    npm --prefix web ci --omit=optional
    npm --prefix web run build
    if ! diff -r web/out internal/server/webui >/dev/null 2>&1; then
        echo "internal/server/webui/ is stale — run 'just web' and commit the result." >&2
        diff -rq web/out internal/server/webui || true
        exit 1
    fi
    echo "embedded web UI is up to date."
