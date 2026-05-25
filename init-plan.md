# Cloud Asset Auditor — Implementation Plan

A spec for Claude Code to build the project end-to-end. Execute the phases in order; each phase is independently shippable and testable.

---

## 0. Language decision: Go (recommended)

**Use Go.** Reasoning specific to this project:

| Concern | Why Go wins for *this* tool |
|---|---|
| Distribution | Single static binary → trivial CLI install, ~15 MB scratch/distroless image vs ~150 MB Python image |
| Kubernetes | `k8s.io/client-go` is the canonical client; Python's `kubernetes` lib is a generated wrapper around it |
| Concurrency | Collecting from OCI + Cloudflare + K8s in parallel is one `errgroup` away; Python needs `asyncio` + sync-API juggling |
| SDK parity | Official SDKs exist in Go for all three: `oci-go-sdk`, `cloudflare-go`, `client-go` |
| CI/CD friendly | Binary in a GitHub Action is faster to set up than `pip install` + cache |
| Web UI | `embed.FS` lets you ship HTML/CSS/JS inside the binary — no static-file mounting |

**Pick Python only if** the maintainers strongly prefer it or you anticipate non-Go contributors. If switching to Python, swap: `cobra` → `click`, `client-go` → `kubernetes`, `cloudflare-go` → `cloudflare`, `oci-go-sdk` → `oci`, `embed.FS` → `importlib.resources`, `goreleaser` → `pyinstaller`/`uv build`. The rest of this plan applies unchanged.

---

## 1. Repository layout

```
cloud-asset-auditor/
├── cmd/
│   └── auditor/
│       └── main.go                 # entrypoint, wires cobra
├── internal/
│   ├── cli/                        # cobra commands: audit, serve, version
│   ├── config/                     # viper-based config + env-var loading
│   ├── core/
│   │   ├── asset.go                # canonical Asset struct
│   │   ├── provider.go             # Provider interface
│   │   └── registry.go             # provider registration
│   ├── providers/
│   │   ├── oci/
│   │   ├── cloudflare/
│   │   └── kubernetes/
│   ├── output/
│   │   ├── json.go
│   │   ├── csv.go
│   │   └── renderer.go             # Renderer interface
│   ├── server/                     # web UI HTTP handlers
│   └── version/                    # ldflags-injected build info
├── web/
│   ├── index.html
│   ├── app.js
│   └── styles.css
├── deploy/
│   ├── docker/
│   │   └── Dockerfile
│   └── helm/
│       └── cloud-asset-auditor/    # standard Helm chart layout
├── .github/
│   ├── workflows/
│   │   ├── ci.yml
│   │   ├── release.yml
│   │   └── docker.yml
│   └── actions/
│       └── audit/                  # reusable composite action
│           └── action.yml
├── docs/
│   ├── configuration.md
│   ├── providers.md
│   └── extending.md
├── examples/
│   └── config.yaml
├── Makefile
├── go.mod
├── .goreleaser.yaml
├── .golangci.yml
└── README.md
```

---

## 2. Core abstractions

These are the contracts every provider and output format must honor. Implement them in Phase 1 *before* touching any provider code.

### 2.1 Canonical Asset

```go
// internal/core/asset.go
type Asset struct {
    Provider    string            `json:"provider"`     // "oci", "cloudflare", "kubernetes"
    AccountID   string            `json:"account_id"`   // tenancy OCID, CF account ID, cluster name
    Region      string            `json:"region,omitempty"`
    Type        string            `json:"type"`         // "compute.instance", "dns.zone", "v1.Pod"
    ID          string            `json:"id"`           // provider-native ID
    Name        string            `json:"name"`
    Status      string            `json:"status,omitempty"`
    CreatedAt   *time.Time        `json:"created_at,omitempty"`
    Tags        map[string]string `json:"tags,omitempty"`
    Raw         json.RawMessage   `json:"raw,omitempty"` // optional full payload, opt-in via --include-raw
}
```

Keep this struct minimal and stable. Provider-specific richness lives in `Raw`.

### 2.2 Provider interface

```go
// internal/core/provider.go
type Provider interface {
    Name() string
    Validate(ctx context.Context) error            // credential/connectivity check
    Collect(ctx context.Context) (<-chan Asset, <-chan error)
}
```

Streaming via channels lets large inventories (thousands of K8s resources) start rendering immediately and keeps memory bounded.

### 2.3 Renderer interface

```go
// internal/output/renderer.go
type Renderer interface {
    Render(ctx context.Context, in <-chan Asset, w io.Writer) error
}
```

---

## 3. Implementation phases

Each phase ends with a working binary and at least one test. Don't merge a phase that breaks the previous one.

### Phase 1 — Foundation (no providers yet)

1. `go mod init github.com/<org>/cloud-asset-auditor`
2. Wire `cobra` with `audit` and `serve` commands (stubs).
3. Wire `viper` config: search order = flag > env (`AUDITOR_*`) > `./auditor.yaml` > `$HOME/.config/auditor.yaml`.
4. Implement `Asset`, `Provider`, `Renderer`, and a `registry` map.
5. Implement JSON renderer (streaming `json.Encoder`, one asset per line in `--stream` mode, otherwise array).
6. Implement CSV renderer (flattens `Tags` to `tag.key=value;key=value` column).
7. Add `version` subcommand using `ldflags` (`-X internal/version.Version=...`).
8. `Makefile` targets: `build`, `test`, `lint`, `run`, `docker`.

**Exit criteria:** `auditor audit --provider none -o json` returns `[]`. `auditor version` works.

### Phase 2 — Cloudflare provider (start here, simplest auth)

- Auth: `CLOUDFLARE_API_TOKEN` only — no legacy email+key path.
- Use `github.com/cloudflare/cloudflare-go/v2`.
- Resources to enumerate (this is "full account inventory"):
  - Zones, DNS records, Workers (scripts + routes), R2 buckets, KV namespaces, D1 databases, Pages projects, Access apps, Tunnels, Load Balancers, Rulesets, Page Rules, Certificates.
- Pattern: one method per resource type that pushes to the asset channel; parent goroutine fans them out under an `errgroup` with `--max-concurrency` (default 5).
- Map each resource → `Asset` with `Type = "cloudflare.<resource>"`.

**Exit criteria:** `auditor audit --provider cloudflare -o csv > assets.csv` produces a non-empty CSV against a test account.

### Phase 3 — OCI provider

- Auth precedence: instance principal → resource principal → config file (`~/.oci/config`) → env vars. The SDK has helpers for this chain; expose `--oci-profile` flag.
- Use `github.com/oracle/oci-go-sdk/v65`.
- Iterate compartments recursively from the tenancy root (the OCI gotcha — most users forget nested compartments).
- Resources: Compute instances, Block volumes, Boot volumes, VCNs, Subnets, Load balancers, Object Storage buckets, Autonomous DBs, DB systems, Functions, Container instances, OKE clusters, Vaults, Policies, Users, Groups, Dynamic groups.
- One goroutine per region from `--oci-regions` (default: home region only; `all` for every subscribed region).

**Exit criteria:** runs against a tenancy with multiple compartments and returns assets from every compartment.

### Phase 4 — Kubernetes provider

- Auth: in-cluster config if `KUBERNETES_SERVICE_HOST` is set, else `KUBECONFIG` / `~/.kube/config`. Expose `--kube-context`.
- Use **dynamic client + discovery** instead of typed clients — this lets you inventory CRDs without code changes:
  ```go
  discoveryClient.ServerPreferredResources()  // every GVR the cluster exposes
  dynamicClient.Resource(gvr).List(...)        // generic list
  ```
- Skip subresources (`status`, `scale`, etc.) and resources the SA can't list (log a warning, don't fail).
- Honor `--kube-namespace` (single ns) and `--kube-exclude-namespaces` (default: `kube-system,kube-public,kube-node-lease`).
- Map each object → `Asset` with `Type = "<group>/<version>.<kind>"`, e.g. `apps/v1.Deployment`.

**Exit criteria:** runs against a kind/minikube cluster and lists pods, services, deployments, and at least one CRD instance.

### Phase 5 — Web UI

- `auditor serve --addr :8080` starts an HTTP server.
- Routes:
  - `GET /` → `index.html` (from `embed.FS`)
  - `GET /api/audit?providers=oci,cloudflare` → SSE stream of assets (one event per asset, plus a `done` event)
  - `GET /api/audit/export?format=csv` → triggers download
  - `GET /healthz` → liveness
- Frontend stack: **vanilla HTML + Alpine.js (15 KB) + a tiny sort/filter library** (`list.js` or hand-rolled). No build step, no bundler, no npm. Everything embedded.
- Table features: column sort, free-text filter, provider/type facets, export-current-view button, sticky header.
- Polling vs SSE: use SSE so users see the count climb in real time during long audits.

**Exit criteria:** browse to `http://localhost:8080`, click "Run audit," see rows stream in, click "Export CSV," get the same data the CLI would emit.

### Phase 6 — Docker

```dockerfile
# deploy/docker/Dockerfile
FROM golang:1.23 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -ldflags="-s -w -X github.com/<org>/cloud-asset-auditor/internal/version.Version=${VERSION}" \
    -o /out/auditor ./cmd/auditor

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/auditor /auditor
USER nonroot:nonroot
ENTRYPOINT ["/auditor"]
CMD ["audit", "--help"]
```

Image must be < 30 MB and run as non-root.

### Phase 7 — Helm chart

`deploy/helm/cloud-asset-auditor/`:

- `Chart.yaml`, `values.yaml`, standard `templates/` dir.
- Two deployment modes via `values.mode`:
  - `cronjob` (default) — periodic audit, writes JSON to a PVC or pushes to S3-compatible storage.
  - `deployment` — long-running web UI behind a Service + optional Ingress.
- Secrets: never inline credentials. Reference an existing Secret via `values.credentials.existingSecret`. Provide an `examples/secret.yaml` showing the expected keys (`CLOUDFLARE_API_TOKEN`, `OCI_CONFIG`, etc.).
- RBAC: when the chart runs in-cluster auditing, create a `ClusterRole` with `get,list` on `*` (read-only). Document the permission surface.
- `helm lint` and `helm template` must pass in CI.

### Phase 8 — GitHub Actions

Three workflows + one reusable action.

**`.github/workflows/ci.yml`** — runs on PRs:
- `go test ./... -race -cover`
- `golangci-lint run`
- `gosec ./...`
- `helm lint deploy/helm/cloud-asset-auditor`
- Build the binary and run `auditor --help` as a smoke test.

**`.github/workflows/release.yml`** — runs on tags `v*`:
- Uses `goreleaser` to build cross-platform binaries (linux/darwin/windows × amd64/arm64), produce checksums, sign with cosign keyless, and create the GitHub Release.

**`.github/workflows/docker.yml`** — runs on tags and `main`:
- Builds and pushes multi-arch image to GHCR with tags: `latest`, `<sha>`, `<semver>`.
- Trivy scan as a separate job; fails on HIGH/CRITICAL unless suppressed via `.trivyignore`.

**`.github/actions/audit/action.yml`** — reusable composite action so other repos can do:
```yaml
- uses: <org>/cloud-asset-auditor/.github/actions/audit@v1
  with:
    providers: cloudflare,kubernetes
    output: assets.json
  env:
    CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
```
Internally it downloads the matching release binary (pinned by action ref) and runs it.

### Phase 9 — Documentation

- `README.md` — install (binary, Docker, Helm, `go install`), quickstart for each provider, screenshot of UI.
- `docs/configuration.md` — every flag, env var, and config-file key. Auto-generate from cobra where possible (`cobra-cli` has a doc generator).
- `docs/providers.md` — per-provider auth setup, required permissions (the *minimum* IAM policy for each), resource list.
- `docs/extending.md` — how to add a new provider in <100 lines. Walk through the `Provider` interface with a worked example.

---

## 4. CLI surface (final shape)

```
auditor audit [flags]
  --provider strings           # one or more: oci, cloudflare, kubernetes (default: all configured)
  --output string              # json|csv (default "json")
  --output-file string         # default stdout
  --stream                     # NDJSON instead of JSON array
  --include-raw                # include full provider payload in each asset
  --max-concurrency int        # per-provider parallelism (default 5)
  --timeout duration           # overall audit timeout (default 10m)
  --config string              # path to config file

  # provider-scoped flags
  --oci-profile string
  --oci-regions strings        # or "all"
  --kube-context string
  --kube-namespace string
  --kube-exclude-namespaces strings

auditor serve [flags]
  --addr string                # default ":8080"
  --auth string                # none|basic|token (default "none"); document that prod should put it behind a real proxy

auditor version
auditor providers              # list configured + their validation status
```

---

## 5. Testing strategy

- **Unit tests** per provider against mocked SDK clients. Use `gomock` or hand-rolled interfaces — don't hit real APIs in unit tests.
- **Integration tests** behind a build tag (`//go:build integration`) that require real credentials; run them in a nightly workflow against a sandbox tenancy, not on every PR.
- **Golden files** for renderers: feed a fixed `[]Asset`, assert the JSON/CSV output byte-for-byte.
- **K8s tests** with `envtest` (controller-runtime's local apiserver) — much faster than spinning up kind.
- Aim for ≥70% coverage on `internal/core` and `internal/output`; providers will be lower because most code is SDK glue.

---

## 6. Things to get right early (cheap now, expensive later)

1. **Streaming everywhere.** If you buffer the full asset list in memory, you'll regret it the first time someone runs this against a real K8s cluster with 50k objects.
2. **Context cancellation.** Every SDK call takes a `context.Context`. Plumb it. `Ctrl+C` should stop in <1s.
3. **Structured logging** with `log/slog` from day one. Log to stderr, never stdout (stdout is reserved for JSON/CSV output when no `--output-file` is given).
4. **No secrets in logs, ever.** Add a redaction helper and use it in every error wrapping site.
5. **Provider failures are partial, not fatal.** If OCI times out, still return Cloudflare results, with an `errors` section in JSON output (and a non-zero but distinct exit code, e.g. 2).
6. **Versioned API surface for the web UI.** `/api/v1/audit`, not `/api/audit`. Future-you will thank present-you.

---

## 7. Suggested execution order for Claude Code

When kicking this off, give Claude Code one phase at a time. Don't paste the whole document and say "go." Recommended prompts:

1. *"Implement Phase 1 from the plan. Skip provider implementations. Show me the resulting tree and `go test ./...` output."*
2. *"Implement Phase 2 (Cloudflare). Mock the SDK in tests."*
3. … and so on.

After Phase 5, the project is already useful and shippable. Phases 6–9 are about making it pleasant for others to consume.
