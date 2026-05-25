# cloud-asset-auditor

Single-binary CLI (and, eventually, web UI) that inventories cloud assets
across OCI, Cloudflare, and Kubernetes into one canonical schema, with
JSON or CSV output.

> **Status: Phase 2 — Cloudflare provider in flight.** Foundation, renderers,
> CLI, and Cloudflare zones + DNS records are shipped. Eleven other Cloudflare
> resource types (R2, KV, Workers, Pages, Access, Tunnels, Load Balancers,
> Rulesets, Page Rules, D1, Certificates) are stubbed in
> `internal/providers/cloudflare/stubs.go` for incremental fill-in.
> OCI (Phase 3) and Kubernetes (Phase 4) are not started. See
> [`init-plan.md`](./init-plan.md) for the full phased plan and
> [`CLAUDE.md`](./CLAUDE.md) for architecture notes.

## Install

Requires **Go 1.23+** and [**just**](https://github.com/casey/just).

```bash
git clone https://github.com/cloud-auditor/cloud-asset-auditor.git
cd cloud-asset-auditor
just tidy    # download deps, generate go.sum
just build   # produces ./bin/auditor
```

Tagged releases with prebuilt binaries and a `go install` path land in
Phase 8.

## Quick start

```bash
# Phase 2: real audit against Cloudflare. Lists zones + DNS records today;
# the other 11 resource types are stubs that emit nothing yet.
export CLOUDFLARE_API_TOKEN=cf-token-with-zone-read-and-dns-read
./bin/auditor audit --provider cloudflare -o json
./bin/auditor audit --provider cloudflare -o csv > assets.csv
./bin/auditor audit --provider cloudflare --include-raw -o json   # keeps the full SDK payload in each Asset.Raw

# No-provider path (useful for smoke tests):
./bin/auditor audit --provider none -o json     # → []
./bin/auditor audit --provider none -o csv      # → header row only

./bin/auditor version
./bin/auditor providers                         # → cloudflare
./bin/auditor --help                            # full CLI surface
./bin/auditor audit --help                      # all audit flags
```

The minimum Cloudflare API-token scopes for the current implementation
are **Zone:Read** and **Zone.DNS:Read** at the account level. As more
resource types come online they'll need additional scopes — the full
permission set will land in `docs/providers.md` (Phase 9).

The complete flag surface (including provider-scoped flags like
`--oci-regions`, `--kube-context`, `--max-concurrency`, `--include-raw`)
is declared from day one so it's stable; the flags wire to real behavior
starting in Phase 2.

## Configuration

Three sources, in precedence order (highest wins):

1. **Flags** — e.g. `-o csv`, `--timeout 5m`
2. **Environment** — prefix `AUDITOR_`, dots and dashes become underscores,
   uppercase. `AUDITOR_OUTPUT_FORMAT=csv`, `AUDITOR_AUDIT_TIMEOUT=5m`.
3. **Config file** — `./auditor.yaml` or `~/.config/auditor.yaml` (or
   `--config <path>`). YAML. A missing file is not an error.

## Output schema

Every asset, regardless of provider, conforms to one canonical struct:

```jsonc
{
  "provider":    "cloudflare",          // "oci" | "cloudflare" | "kubernetes"
  "account_id":  "<tenancy / account / cluster>",
  "region":      "<optional>",
  "type":        "cloudflare.zone",     // provider.resource
  "id":          "<provider-native id>",
  "name":        "<human-readable name>",
  "status":      "<optional>",
  "created_at":  "2025-01-02T03:04:05Z",
  "tags":        { "env": "prod" },
  "raw":         { /* full provider payload — opt in with --include-raw */ }
}
```

CSV mode emits the same fields as columns and flattens `tags` to
`k1=v1;k2=v2` (keys sorted) into a single column.

## Development

| Recipe              | What it does                                                    |
| ------------------- | --------------------------------------------------------------- |
| `just build`        | Build `./bin/auditor` with version metadata baked in via ldflags |
| `just test`         | `go test -race -cover ./...`                                    |
| `just test-update`  | Regenerate renderer golden files (use after intentional output changes) |
| `just lint`         | `golangci-lint run`                                             |
| `just run -- <args>`| `go run ./cmd/auditor <args>` — the `--` keeps just from eating flags |
| `just tidy`         | `go mod tidy`                                                   |
| `just smoke`        | Build, then assert the Phase 1 exit criteria                    |
| `just docker`       | Multi-stage image build — wired in Phase 6, fails until then    |

Run `just` with no args to list recipes.

### Adding a provider

Until Phase 2 lands, there's no worked example, but the contract is small.
A provider implements:

```go
type Provider interface {
    Name() string
    Validate(ctx context.Context) error
    Collect(ctx context.Context) (<-chan Asset, <-chan error)
}
```

Channels are required, not optional — they're what keeps memory bounded
against large inventories (think 50k+ Kubernetes objects). Register the
provider in a package `init()`:

```go
core.Register("cloudflare", func() (core.Provider, error) {
    return cloudflare.New(/* config */)
})
```

A full extending guide ships in Phase 9.

## Roadmap

| Phase | Status   | Scope |
| ----- | -------- | ----- |
| 1 — Foundation              | shipped  | Core types, JSON / CSV renderers, CLI skeleton, version, justfile |
| 2 — Cloudflare provider     | partial  | Zones + DNS records implemented; R2 / KV / Workers / D1 / Pages / Access / Tunnels / Load Balancers / Rulesets / Page Rules / Certificates stubbed |
| 3 — OCI provider            | planned  | Compartment-recursive, multi-region, instance + resource principal auth |
| 4 — Kubernetes provider     | planned  | Dynamic-client discovery so CRDs come along for free |
| 5 — Web UI                  | planned  | SSE stream, embedded HTML + Alpine, no build step |
| 6 — Docker                  | planned  | Distroless multi-stage, < 30 MB, non-root |
| 7 — Helm chart              | planned  | CronJob and Deployment modes, BYO secrets |
| 8 — GitHub Actions          | planned  | CI, goreleaser, multi-arch GHCR image, reusable composite action |
| 9 — Docs                    | planned  | Per-provider IAM minimums, extending guide, generated CLI docs |
| 10 — Network topology       | planned  | Infer edges between assets; trace `DNS → security → LB → gateway → service` as JSON / Graphviz / Cytoscape view |

## License

No `LICENSE` file is committed yet — all rights reserved until one lands.
