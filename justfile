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

# Generated from the demo fixture, never a real tenancy — see docs/diagrams/README.md.
#
# Regenerate docs/diagrams/ (needs d2 + rsvg-convert).
diagrams:
    #!/usr/bin/env bash
    set -euo pipefail
    command -v d2 >/dev/null || { echo "d2 not found — brew install d2" >&2; exit 1; }
    command -v rsvg-convert >/dev/null || { echo "rsvg-convert not found — brew install librsvg" >&2; exit 1; }
    out=docs/diagrams
    tmp=$(mktemp -d)
    trap 'rm -rf "$tmp"' EXIT
    mkdir -p "$out"
    export AUDITOR_DEMO_DURATION=0
    # Built once rather than four `go run`s — and `go run` swallows the real
    # exit code (it exits 1 and prints "exit status N"), which matters here:
    # the demo reports three simulated non-fatal provider errors on purpose, so
    # it exits 2 (partial failure) every time. That is success; anything else
    # is not, and with `go run` the two are indistinguishable.
    go build -o "$tmp/auditor" ./cmd/auditor
    topo() {
        local rc=0
        "$tmp/auditor" topology --demo --group-by provider "$@" || rc=$?
        [ "$rc" -eq 0 ] || [ "$rc" -eq 2 ]
    }
    for level in high:medium low:low; do
        name=${level%%:*}; detail=${level#*:}
        topo --detail "$detail" -o d2         > "$tmp/$name.d2"
        topo --detail "$detail" -o excalidraw > "$out/northwind-$name-level.excalidraw"
        d2 --layout elk --pad 40 "$tmp/$name.d2" "$out/northwind-$name-level.svg" >/dev/null
        chmod 644 "$out/northwind-$name-level.svg"
    done
    rsvg-convert -w 2200 "$out/northwind-high-level.svg" -o "$out/northwind-high-level.png"
    rsvg-convert -w 3000 "$out/northwind-low-level.svg"  -o "$out/northwind-low-level.png"
    echo "diagrams regenerated in docs/diagrams/"

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

# ---------------------------------------------------------------------------
# Price book (Oracle's public list prices → the embedded books/oci.yaml)
# ---------------------------------------------------------------------------

# internal/pricing/books/oci.yaml is COMMITTED and go:embed-ed, so `go build`,
# CI, goreleaser and the Docker image never touch the network — the binary
# prices exactly as well air-gapped as online, and says which day its numbers
# are from. Run this after adding or changing a rate id, SKU or rule in that
# file, and commit the regenerated book together with the recorded feed fixture
# in internal/pricing/testdata/.
#
# Only rates[].amount, rates[].tier_note and books[].vintage are machine-written
# — rules and shapes stay hand-curated, because a rule nobody wrote is a number
# nobody can defend. Each amount is taken from the SKU's MARGINAL tier: Oracle
# encodes Always Free allowances as a $0.00 first tier, and reading that one
# reports your first load balancer as free forever.
#
# It fails on the first declared SKU the feed no longer carries, which is how a
# renamed part number stops a release instead of decaying into a stale price.
#
# Re-price internal/pricing/books/oci.yaml from Oracle's public price list.
prices:
    go run internal/pricing/genoci.go
    @echo "commit books/oci.yaml together with internal/pricing/testdata/oci-feed.json.gz"

# Deliberately NOT a CI gate, unlike `just web-verify`. Oracle republishes the
# feed on its own schedule, so gating pull requests on it would turn an
# unrelated upstream price change into a red build on every open PR. The
# deterministic check — that every committed amount really is the marginal tier
# of the committed feed fixture — is TestPrices_UseMarginalTier, which already
# runs offline in the `test` job.
#
# Run this from a scheduled job instead, and open a PR when it reports drift.
#
# Report whether Oracle's live prices have moved away from the committed book.
prices-check:
    #!/usr/bin/env bash
    set -euo pipefail
    go run internal/pricing/genoci.go
    if git diff --quiet -- internal/pricing/books internal/pricing/testdata; then
        echo "price book matches Oracle's current feed."
        exit 0
    fi
    echo "Oracle's feed has moved — review and commit:" >&2
    git --no-pager diff --stat -- internal/pricing/books internal/pricing/testdata >&2
    git --no-pager diff -- internal/pricing/books/oci.yaml >&2
    exit 1
