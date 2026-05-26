# cloud-asset-auditor

Single-binary CLI (and, eventually, web UI) that inventories cloud assets
across OCI, Cloudflare, and Kubernetes into one canonical schema, with
JSON or CSV output.

> **All phases shipped.** Foundation, JSON / CSV renderers, CLI, three
> providers (Cloudflare zones+DNS / OCI compartments+Compute+LBs /
> Kubernetes universal), web UI (`auditor serve`), Docker image (distroless
> static, non-root), Helm chart, GitHub Actions (CI + goreleaser + multi-arch
> GHCR + Trivy + reusable `audit` action), docs, and the topology graph
> (`auditor topology` → JSON / DOT / Mermaid plus `/api/v1/topology`).
> Remaining work is the per-provider stubbed resource types
> (11 Cloudflare, 15 OCI). See [`init-plan.md`](./init-plan.md) for the
> full plan and [`CLAUDE.md`](./CLAUDE.md) for architecture notes.

## Install

Three options, in increasing order of "I want it working five minutes ago":

**1. Prebuilt release (recommended)** — cross-compiled for linux / macOS / windows × amd64 / arm64, with cosign-signed SHA256 checksums:

```bash
curl -L https://github.com/cloud-auditor/cloud-asset-auditor/releases/latest/download/auditor_$(uname -s | tr '[:upper:]' '[:lower:]')_$(uname -m | sed s/x86_64/amd64/).tar.gz | tar xz
./auditor version
```

**2. `go install`** — needs Go 1.26+:

```bash
go install github.com/cloud-auditor/cloud-asset-auditor/cmd/auditor@latest
```

**3. From source** — needs Go 1.26+ and [`just`](https://github.com/casey/just):

```bash
git clone https://github.com/cloud-auditor/cloud-asset-auditor.git
cd cloud-asset-auditor
just tidy    # download deps, generate go.sum
just build   # produces ./bin/auditor
```

The Docker image (`ghcr.io/cloud-auditor/cloud-asset-auditor:latest`)
and Helm chart are documented under [Container](#container) and
[Kubernetes (Helm)](#kubernetes-helm) below.

## Quick start

```bash
# Cloudflare (Phase 2): zones + DNS today; other 11 types are stubs.
export CLOUDFLARE_API_TOKEN=cf-token-with-zone-read-and-dns-read
./bin/auditor audit --provider cloudflare -o csv > cf.csv

# OCI (Phase 3): compartment recursion + Compute instances + Load Balancers
# today; 15 other resource types stubbed.
#   Auth chain (auto-detected): instance principal → resource principal
#   → ~/.oci/config → OCI_* env vars
./bin/auditor audit --provider oci -o json                       # home region only
./bin/auditor audit --provider oci --oci-regions all -o csv      # every subscribed region
./bin/auditor audit --provider oci --oci-profile PROD            # named profile

# Kubernetes (Phase 4): every resource type the cluster exposes — built-ins
# and CRDs — via dynamic discovery. No need to list "what to scan"; the
# cluster tells us.
#   Auth: in-cluster when KUBERNETES_SERVICE_HOST is set, else KUBECONFIG /
#   ~/.kube/config; --kube-context overrides current-context.
./bin/auditor audit --provider kubernetes -o json
./bin/auditor audit --provider kubernetes --kube-context kind-dev -o csv
./bin/auditor audit --provider kubernetes --kube-namespace prod
./bin/auditor audit --provider kubernetes --kube-exclude-namespaces kube-system,kube-public,kube-node-lease

./bin/auditor audit --include-raw -o json                        # any provider, with full SDK payloads

# No-provider path (useful for smoke tests):
./bin/auditor audit --provider none -o json     # → []
./bin/auditor audit --provider none -o csv      # → header row only

./bin/auditor version
./bin/auditor providers                         # → cloudflare\nkubernetes\noci
./bin/auditor --help                            # full CLI surface
./bin/auditor audit --help                      # all audit flags
```

## Web UI

`auditor serve` runs an embedded single-page app + JSON/SSE API. The
operator's credentials come from the environment at startup (same env
vars / config files as the CLI); the browser never receives them. The
frontend can pick which registered providers to run but cannot supply
new credentials.

```bash
./bin/auditor serve                                   # → http://localhost:8080, auth=none
./bin/auditor serve --addr 127.0.0.1:9090 --auth basic
#   With AUDITOR_BASIC_USER / AUDITOR_BASIC_PASS env vars
./bin/auditor serve --auth token
#   With AUDITOR_API_TOKEN env; client sends `Authorization: Bearer <token>`
```

Endpoints:

| Path                                  | Purpose                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `GET /`                               | Embedded SPA — provider checkboxes, "Run audit" button, streamed table, filter / sort / facets   |
| `GET /healthz`                        | Liveness — always 200, always open (load-balancer probes don't need auth)                        |
| `GET /api/v1/providers`               | `{providers: [...], auth_mode: "..."}`                                                           |
| `GET /api/v1/audit?providers=a,b`     | SSE stream: `meta` → `asset`* → `done`. Optional `init_error` / `error` events interleaved       |
| `GET /api/v1/audit/export?format=csv` | Synchronous download of CSV / JSON / NDJSON — same bytes the CLI emits                           |

Production deployments should sit behind a real reverse proxy (TLS
termination, rate-limiting, IP allowlist). Built-in `basic` / `token`
are a backstop for unmanaged setups, not a substitute.

## Container

```bash
just docker                                # → cloud-asset-auditor:<version> + :latest
docker images cloud-asset-auditor:latest   # confirm size

# Print help (default CMD).
docker run --rm cloud-asset-auditor:latest

# CLI mode — credentials passed via env / mounted config.
docker run --rm \
  -e CLOUDFLARE_API_TOKEN=$CLOUDFLARE_API_TOKEN \
  cloud-asset-auditor:latest audit --provider cloudflare -o json

# Web UI mode — port 8080 + a healthcheck.
docker run --rm -p 8080:8080 cloud-asset-auditor:latest serve --addr :8080
curl http://localhost:8080/healthz       # → ok

# Read-only filesystem + non-root, the way Kubernetes will run it.
docker run --rm --read-only --user 65532:65532 \
  cloud-asset-auditor:latest audit --provider none -o json
```

Image notes:

- **Base**: `gcr.io/distroless/static-debian12:nonroot` (~2 MB; no shell, no package manager, no glibc).
- **User**: `nonroot` (UID/GID 65532). Mounted volumes (kubeconfig, OCI config) must be readable by that UID.
- **Architecture**: build inherits `$TARGETARCH` from `docker build --platform`; the CI workflow in Phase 8 will produce multi-arch (`linux/amd64`, `linux/arm64`) tags.
- **Size**: ~75 MB. The plan called for <30 MB; in practice the three production SDKs (cloudflare-go/v4, oci-go-sdk/v65, k8s client-go) make that target unachievable without ripping providers out. Documented in `CLAUDE.md` and the Dockerfile.

## Kubernetes (Helm)

The chart at [`deploy/helm/cloud-asset-auditor/`](./deploy/helm/cloud-asset-auditor/)
deploys the same image in one of two shapes:

| `mode` | Shape | Use when… |
| ------ | ----- | --------- |
| `cronjob` (default) | `batch/v1.CronJob` | You want periodic snapshots written to logs or a PVC |
| `deployment` | `apps/v1.Deployment` + Service (+ optional Ingress) | You want a browser-accessible UI for ad-hoc audits |

```bash
kubectl create namespace auditor

# 1. Credentials Secret (see chart README for the recognized keys).
kubectl -n auditor apply -f deploy/helm/cloud-asset-auditor/examples/secret.yaml

# 2a. CronJob mode (every 6h by default; tune cronjob.schedule).
helm install auditor deploy/helm/cloud-asset-auditor -n auditor \
  -f deploy/helm/cloud-asset-auditor/examples/values-cronjob.yaml

# 2b. OR Deployment mode (long-running serve behind Ingress).
helm install auditor deploy/helm/cloud-asset-auditor -n auditor \
  -f deploy/helm/cloud-asset-auditor/examples/values-deployment.yaml
```

The chart provisions a **read-only-everywhere** ClusterRole (`get`, `list`
on `*`/`*`) by default — necessary for the Kubernetes provider's dynamic
discovery to inventory CRDs. Disable via `rbac.create=false` and bind the
chart's ServiceAccount to a narrower role; the provider tolerates
Forbidden responses per-resource.

Full chart docs and the complete values reference live in
[`deploy/helm/cloud-asset-auditor/README.md`](./deploy/helm/cloud-asset-auditor/README.md).

## CI / Release

GitHub Actions live in `.github/workflows/`:

| Workflow      | Trigger                          | What it does                                                                                                    |
| ------------- | -------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `ci.yml`      | PR + push to `main`              | Parallel jobs: `go test -race -cover`, golangci-lint, gosec, helm lint + template, build + smoke (`audit --provider none -o json == []`) |
| `release.yml` | Push of a `v*` tag               | `goreleaser` cross-builds (linux / darwin / windows × amd64 / arm64) + SHA256 checksums + cosign keyless OIDC signature + SBOM (syft) + GitHub Release |
| `docker.yml`  | Push to `main` + `v*` tags + PRs | Buildx multi-arch (linux/amd64 + linux/arm64) image push to `ghcr.io/cloud-auditor/cloud-asset-auditor` with cosign signing, then Trivy scan (HIGH/CRITICAL gate; suppress via `.trivyignore`) with SARIF upload to GitHub Security |

## Topology

`auditor topology` walks the inventory and infers the request-path graph
between assets: DNS → security → cloud LB → cluster gateway → Service.
Edges carry a `confidence` field (`exact` for same-cluster lookups,
`heuristic` for cross-cloud IP/hostname matches) so the rendered graph
makes its guesses visible.

```bash
# Render to SVG via Graphviz (the typical runbook flow).
auditor topology -o dot | dot -Tsvg > flow.svg

# Trace a single hostname.
auditor topology --hostname api.example.com -o mermaid

# Editable hand-drawn diagram — drop the file into excalidraw.com or
# the Excalidraw desktop app and drag nodes around; arrows stay attached.
auditor topology -o excalidraw > topology.excalidraw

# Programmatic consumers.
auditor topology -o json | jq '.edges[] | select(.kind == "lb-backend")'

# Or hit the API. Same format= query param picks the renderer.
curl 'http://localhost:8080/api/v1/topology?hostname=api.example.com' | jq
curl 'http://localhost:8080/api/v1/topology?format=excalidraw' -o topology.excalidraw
```

The subcommand forces `--include-raw` on providers internally so the
Kubernetes resolvers can parse Ingress / HTTPRoute / Service payloads.
The rendered output omits `raw` to stay readable.

The Cytoscape.js interactive view init-plan.md §3 Phase 10 envisioned
is deliberately not vendored — same rationale as the vanilla-JS
frontend choice in Phase 5. The JSON endpoint exists precisely so an
out-of-tree dashboard can build whatever interactive view it wants.

### Reusable composite action

The action at `.github/actions/audit/action.yml` lets other repos run
an audit in one step:

```yaml
- uses: cloud-auditor/cloud-asset-auditor/.github/actions/audit@v1
  with:
    providers: cloudflare,kubernetes
    output-file: assets.json
  env:
    CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
    KUBECONFIG: ${{ runner.temp }}/kubeconfig
```

The action downloads the matching release tarball (pinned by the action
ref, with a fallback to the latest release when the ref isn't a semver
tag), runs the audit, and uploads the output as a workflow artifact.

Minimum permissions for what's implemented today:

- **Cloudflare**: API token with **Zone:Read** + **Zone.DNS:Read** at the account level.
- **OCI**: a policy granting `inspect compartments`, `read all-resources` (or at least `read instances` + `read load-balancers`) over the tenancy or compartments you want scanned.
- **Kubernetes**: a ClusterRole with `get,list` on `*` (read-only) is the simplest. The provider gracefully degrades on individual resource types the SA can't list (logs them, keeps going), so a narrower role still works — you just won't see what you can't read.

The full per-resource permission matrix lands in `docs/providers.md` (Phase 9).

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
| 3 — OCI provider            | partial  | Compartment recursion + region resolution + Compute + Load Balancers implemented; Block / Boot volumes, VCNs, Subnets, Object Storage, Autonomous DBs, DB Systems, Functions, Container Instances, OKE, Vaults, Policies, Users, Groups, Dynamic Groups stubbed |
| 4 — Kubernetes provider     | shipped  | Dynamic-client + discovery — every built-in resource type and every CRD with no per-resource code. `--kube-context`, `--kube-namespace`, `--kube-exclude-namespaces` honored; per-GVR Forbidden tolerated; aggregated-API discovery failures degrade to warnings |
| 5 — Web UI                  | shipped  | Embedded SPA + JSON/SSE API. `auditor serve --addr ... --auth none\|basic\|token`. Streamed asset table, filter / sort / type+provider facets, CSV/JSON export, graceful shutdown. Plain JS rather than the planned Alpine.js — keeps the binary fully self-contained |
| 6 — Docker                  | shipped  | Multi-stage build → `gcr.io/distroless/static-debian12:nonroot`. Non-root (UID 65532), reproducible-ish (`-trimpath`, ldflags-injected version), accepts `--platform` for multi-arch. ~75 MB rather than the plan's <30 MB target (cloudflare-go/v4 + oci-go-sdk/v65 + k8s client-go are large) |
| 7 — Helm chart              | shipped  | `deploy/helm/cloud-asset-auditor/` — CronJob (default, optional PVC for persisted output) and Deployment (Service + optional Ingress) modes. BYO credentials Secret (`existingSecret`). Read-only `get,list` ClusterRole (overridable). Example values for both modes |
| 8 — GitHub Actions          | shipped  | `ci.yml` (test + lint + gosec + helm lint + smoke), `release.yml` (goreleaser cross-build + cosign keyless + SBOM), `docker.yml` (multi-arch GHCR push + cosign image sign + Trivy SARIF), reusable `actions/audit` composite |
| 9 — Docs                    | shipped  | [`docs/configuration.md`](./docs/configuration.md), [`docs/providers.md`](./docs/providers.md), [`docs/extending.md`](./docs/extending.md). README install paths cover prebuilt release / `go install` / from-source / Docker / Helm |
| 10 — Network topology       | shipped  | `auditor topology` subcommand → JSON / DOT / Mermaid / **Excalidraw** (LR-layered layout, color-coded per provider, dashed arrows for heuristic edges, deterministic seeds for diff-friendly output). Resolvers: `dnsToTarget` (cross-cloud heuristic), `wafBinding` (skeleton), `lbToGateway` (OCI LB → K8s Service by external IP), `gatewayToService` (Ingress / HTTPRoute → backing Service, exact). `/api/v1/topology` returns JSON by default or any format via `?format=` |

## Docs

- **[`docs/configuration.md`](./docs/configuration.md)** — every flag, env var, and config-file key, with precedence rules and exit codes
- **[`docs/providers.md`](./docs/providers.md)** — per-provider auth setup, minimum permission templates (CF token scopes, OCI policy snippets, K8s ClusterRole YAML), and the per-resource implementation matrix
- **[`docs/extending.md`](./docs/extending.md)** — step-by-step worked example for adding a new provider
- **[`CLAUDE.md`](./CLAUDE.md)** — architecture notes for contributors (and for future Claude Code sessions)
- **[`init-plan.md`](./init-plan.md)** — original phased implementation spec

## License

No `LICENSE` file is committed yet — all rights reserved until one lands.
