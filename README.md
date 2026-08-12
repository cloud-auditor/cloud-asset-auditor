# cloud-asset-auditor

Single-binary CLI + web UI that inventories cloud assets across OCI,
Cloudflare, Kubernetes, GCP, NetBird, and Tailscale into one canonical schema —
JSON, CSV, Excel (XLSX), or a self-contained HTML report — with inferred
network **and traffic-flow** topology graphs at high or low level, an
interactive in-browser diagram, live dashboards, and snapshot drift detection
(`auditor diff`).

> **All phases shipped.** Foundation, JSON / CSV / XLSX renderers, CLI, three
> providers (Cloudflare zones+DNS / OCI all resource types /
> Kubernetes universal), web UI (`auditor serve`), Docker image (distroless
> static, non-root), Helm chart, GitHub Actions (CI + goreleaser + multi-arch
> GHCR + Trivy + reusable `audit` action), docs, and the topology graph
> (`auditor topology` → JSON / DOT / Mermaid plus `/api/v1/topology`).
> Every provider's resource types are fully implemented — Cloudflare,
> OCI, and Kubernetes all collect their complete catalogs. See
> [`init-plan.md`](./init-plan.md) for the full plan and
> [`CLAUDE.md`](./CLAUDE.md) for architecture notes.

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
# Cloudflare (Phase 2): accounts, zones, DNS, R2, KV, Workers, D1, Pages,
# Access apps, Tunnels, certificates, Rulesets, Page Rules, Load Balancers.
export CLOUDFLARE_API_TOKEN=cf-token-with-read-scopes
./bin/auditor audit --provider cloudflare -o csv > cf.csv

# OCI (Phase 3): compartment recursion + all resource types (compute,
# networking, storage, object storage, databases, functions, container
# instances, OKE, vaults, IAM).
#   Auth chain (auto-detected): instance principal → resource principal
#   → ~/.oci/config → OCI_* env vars
./bin/auditor audit --provider oci -o json                       # every subscribed region (default)
./bin/auditor audit --provider oci --oci-regions me-jeddah-1 -o csv  # narrow to one region
./bin/auditor audit --provider oci --oci-compartments Production -o csv  # one compartment + its children (by name or OCID)
./bin/auditor audit --provider oci --oci-profile PROD            # named profile
# Excel workbook, one worksheet per region/compartment (tab = "region (compartment)"):
./bin/auditor audit --provider oci \
  -o xlsx --sheet-by region+tag:compartment_id --output-file oci-assets.xlsx

# Kubernetes (Phase 4): every resource type the cluster exposes — built-ins
# and CRDs — via dynamic discovery. No need to list "what to scan"; the
# cluster tells us.
#   Auth: in-cluster when KUBERNETES_SERVICE_HOST is set, else KUBECONFIG /
#   ~/.kube/config; --kube-context overrides current-context.
./bin/auditor audit --provider kubernetes -o json
./bin/auditor audit --provider kubernetes --kube-context kind-dev -o csv
# Inventory several clusters in one run (or "all" for every kubeconfig context);
# each asset keeps its origin cluster in account_id.
./bin/auditor audit --provider kubernetes --kube-contexts prod-us,prod-eu -o csv
./bin/auditor audit --provider kubernetes --kube-namespace prod
./bin/auditor audit --provider kubernetes --kube-exclude-namespaces kube-system,kube-public,kube-node-lease
# Drop high-volume, ephemeral Event objects (skipped at discovery, never listed):
./bin/auditor audit --provider kubernetes --kube-exclude-events -o csv
# Excel: a sheet per namespace, a Summary sheet up front, Helm release Secrets dropped:
./bin/auditor audit --provider kubernetes --kube-exclude-helm-secrets \
  -o xlsx --sheet-by tag:namespace --summary --output-file k8s-assets.xlsx

# GCP: every resource type across a project / folder / org via the Cloud Asset
# Inventory API (one call — no per-service wiring). Auth is ADC (service-account
# key, gcloud user creds, or workload identity); needs roles/cloudasset.viewer.
export GOOGLE_CLOUD_PROJECT=my-project
./bin/auditor audit --provider gcp -o json
./bin/auditor audit --provider gcp --gcp-scope organizations/123456 -o csv  # whole org

# NetBird (WireGuard mesh / zero-trust networking): peers, groups, policies,
# routes, networks, DNS nameserver groups, setup keys, users, posture checks,
# account — via the REST Management API.
#   Auth: a Personal Access Token (nbp_…) in NETBIRD_API_TOKEN.
export NETBIRD_API_TOKEN=nbp_your_personal_access_token
./bin/auditor audit --provider netbird -o json
# Point at a self-hosted Management API instead of NetBird cloud:
./bin/auditor audit --provider netbird \
  --netbird-management-url https://netbird.example.com -o csv

# Tailscale (WireGuard mesh / zero-trust): devices, users, auth keys, DNS, and
# the tailnet policy file (ACL rules become traffic-flow edges in `topology`).
export TAILSCALE_API_KEY=tskey-api-xxxxxxxxxxxx
./bin/auditor audit --provider tailscale -o json
# A token that can reach several tailnets, or a self-hosted control plane:
./bin/auditor audit --provider tailscale \
  --tailscale-tailnet example.com --tailscale-api-url https://headscale.internal

./bin/auditor audit --include-raw -o json                        # any provider, with full SDK payloads

# Local SQLite (--db, default ~/.config/auditor/auditor.db) backs two things:
#  1) An audit cache so you don't re-pull every run:
./bin/auditor audit --provider netbird --cache -o json           # write the snapshot
./bin/auditor audit --provider netbird --cache-max-age 1h -o json # reuse it if <1h old (skips the API)
#  2) An encrypted secrets vault (AES-256-GCM) so creds load automatically:
export AUDITOR_SECRETS_PASSPHRASE='choose-a-passphrase'
./bin/auditor secrets set NETBIRD_API_TOKEN nbp_xxx              # stored encrypted at rest
./bin/auditor audit --provider netbird                          # token loaded from the vault, no export needed

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

The frontend is a Next.js app (TypeScript, App Router) built as a
**static export** and embedded with `go:embed` — so the binary stays
self-contained: no Node runtime, no CDN, no sidecar. Source lives in
[`web/`](./web); run `just web` after changing it and commit the regenerated
`internal/server/webui/`.

Four pages:

- **Dashboard** — inventory shape while the audit streams: stat tiles
  with a live arrival sparkline, a provider composition ring, ranked
  type / region bars, and a per-provider collection-health panel
  (count, duration, errors).
- **Assets** — the full inventory in a windowed table that stays smooth
  at 50k rows: search, provider / type / region / status facets,
  sortable columns, density and column controls, and a detail drawer
  with every tag and the raw provider payload. Exports to CSV / JSON /
  XLSX / HTML.
- **Topology** — interactive force-directed diagram: curved edges
  coloured by kind, per-type glyphs, group hulls, animated traffic-flow
  edges, minimap, node search, and an inspector that lists a node's
  neighbours. Two build paths: a fresh raw-bearing audit, or an instant
  graph from the assets already streamed. Exports to DOT / Mermaid /
  D2 / GraphML / Excalidraw / draw.io / HTML.
- **Exposure** — reachability, rendered as attack-path chains grouped by
  destination: internet exposure, what can reach an asset, what an asset
  can reach, or every route between two.

Everything is keyboard-reachable, and **⌘K** opens a command palette that
can navigate, start or stop a run, toggle providers, switch theme, jump to
an asset, or grab any export. Light and dark themes both ship; the choice
persists and defaults to the system setting.

```bash
./bin/auditor serve                                   # → http://localhost:8080, auth=none
./bin/auditor serve --demo                            # no credentials needed — synthetic inventory
./bin/auditor serve --addr 127.0.0.1:9090 --auth basic
#   With AUDITOR_BASIC_USER / AUDITOR_BASIC_PASS env vars
./bin/auditor serve --auth token
#   With AUDITOR_API_TOKEN env; client sends `Authorization: Bearer <token>`
```

`--demo` is the fastest way to see what the tool does: it serves a complete
fictional multi-cloud inventory (~590 assets, every edge kind represented)
with nothing to configure. See [docs/providers.md](./docs/providers.md#demo-data).

Endpoints:

| Path                                  | Purpose                                                                                          |
| ------------------------------------- | ------------------------------------------------------------------------------------------------ |
| `GET /`                               | Embedded SPA (Dashboard / Assets / Topology / Exposure). `?run=1` starts an audit on load; `&providers=` / `&kube_contexts=` scope it |
| `GET /healthz`                        | Liveness — always 200, always open (load-balancer probes don't need auth)                        |
| `GET /metrics`                        | Prometheus metrics — always open (scraper semantics)                                             |
| `GET /api/v1/openapi.yaml`            | OpenAPI 3.1 spec for everything under `/api/v1` — auth-exempt                                    |
| `GET /api/v1/providers`               | `{providers: [...], auth_mode: "..."}`                                                           |
| `GET /api/v1/kube/contexts`           | `{contexts: [...], current: "..."}` from the server's kubeconfig (empty when none / in-cluster)  |
| `GET /api/v1/audit?providers=a,b`     | SSE stream: `meta` → `asset`* → `done`. Optional `init_error` / `error` events interleaved. `&kube_contexts=ctx-a,ctx-b` (or `all`) scopes the K8s scan |
| `GET /api/v1/audit/export?format=csv` | Synchronous download — `json` / `ndjson` / `csv` / `html` (report) / `xlsx`                      |
| `GET /api/v1/topology`                | Runs an audit, returns the inferred graph (`?format=json\|dot\|mermaid\|d2\|graphml\|excalidraw\|html`, `?group_by=provider\|account\|region`) |
| `POST /api/v1/topology`               | Same graph engine, but builds from assets in the request body — no audit, instant                |
| `GET /api/v1/reach`                   | Reachability query over the graph (`?exposed=true`, `?from=`, `?to=`, `?kinds=`, `?max_hops=`); same format set as `topology` |
| `POST /api/v1/reach`                  | Same query, built from assets in the request body — no audit, instant                            |

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

Two generated diagrams of the demo estate live in
[`docs/diagrams/`](./docs/diagrams) — a high-level view (one box per provider
and resource type) and a low-level one (every asset), each as `.excalidraw`,
`.svg` and `.png`. Regenerate with `just diagrams`. They are built from the
synthetic demo fixture on purpose: a topology diagram of a real estate is a
complete map of someone's infrastructure and does not belong in a public repo.


`auditor topology` walks the inventory and infers the request-path graph
between assets: DNS → security → cloud LB → cluster gateway → Service →
backing Pods, alongside the OCI network backbone (subnet / gateway / OKE
cluster → VCN, network-LB → subnet). Edges carry a `confidence` field
(`exact` for authoritative same-provider joins, `heuristic` for cross-cloud
IP/hostname matches) so the rendered graph makes its guesses visible.

On top of those request paths it derives **traffic-flow** edges from policy —
Kubernetes NetworkPolicies, Tailscale ACL rules, and NetBird policy rules —
rendered green for `traffic-allow` and red for `traffic-deny`, so a firewall's
denials never read as reachability. Each rule stays in the graph as a node
(`source → rule → destination`), which keeps a catch-all "allow everything"
rule linear instead of a source × destination cross-product.

`--detail low|medium|high` picks the altitude: every asset, one node per
group + resource type, or one box per provider for the executive network
diagram. See [docs/configuration.md](./docs/configuration.md#auditor-topology).

```bash
# Render to SVG via Graphviz (the typical runbook flow).
auditor topology -o dot | dot -Tsvg > flow.svg

# Cluster nodes by cloud (or account / region) for a readable big-picture view.
auditor topology -o dot --group-by provider | dot -Tsvg > flow.svg

# High-level network diagram: one box per provider, arrows weighted by how many
# underlying relationships they stand for. (--detail medium keeps resource types.)
auditor topology --detail high -o dot | dot -Tsvg > overview.svg

# Just the traffic-flow view — who may reach whom, from Kubernetes
# NetworkPolicies, Tailscale ACLs, and NetBird policies.
auditor topology -o json | jq '.edges[] | select(.kind | startswith("traffic-"))'

# Trace a single hostname.
auditor topology --hostname api.example.com -o mermaid

# Modern auto-layout via D2 (d2lang.com); containers mirror --group-by.
auditor topology -o d2 --group-by provider > topology.d2 && d2 topology.d2 topology.svg

# GraphML for desktop graph tools — import into yEd, Gephi, or Cytoscape and
# color / filter by the provider, region, type, and status node attributes.
auditor topology -o graphml > topology.graphml

# Editable diagram with per-service icons — drop the file into excalidraw.com
# or the Excalidraw desktop app and drag cards around; each node carries an
# embedded service glyph (DNS, load balancer, database, …) and the icon +
# label + box move together; arrows stay attached.
auditor topology -o excalidraw > topology.excalidraw

# Standalone interactive diagram — one self-contained HTML file with the
# same force-directed viewer the web UI ships; opens offline, shareable.
auditor topology -o html > topology.html

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
frontend choice in Phase 5. Instead, the web UI's **Topology tab**
renders an interactive force-directed diagram with hand-rolled SVG
(pan / zoom / drag / details panel), and the JSON endpoint exists so
an out-of-tree dashboard can build whatever view it wants on top.

## Reachability

`auditor reach` answers the questions an auditor actually asks, over the graph
`topology` builds:

```bash
auditor reach --exposed                       # what can the internet reach?
auditor reach --to '*postgres*'               # what can reach the database?
auditor reach --from api.example.com --to '*pod*'   # trace every route
auditor reach --to '*db*' --kinds traffic-allow     # only what policy permits
auditor reach --exposed --exit-code           # CI gate
```

Every answer is a path, not a yes/no — the useful part of "yes, the internet
can reach your database" is the hop list that makes it true:

```
1. postgres-0 (v1.Pod)  (5 hops)
     api.example.com (cloudflare.dns_record) --[dns ~]--> prod-lb (oci.load_balancer)
       prod-lb (oci.load_balancer) --[lb-backend ~]--> api (v1.Service)
         api (v1.Service) --[service-backend]--> api-abc (v1.Pod)
           api-abc (v1.Pod) --[traffic-allow:5432]--> db-allow (NetworkPolicy)
             db-allow (NetworkPolicy) --[traffic-allow:5432]--> postgres-0 (v1.Pod)
```

Two deliberate behaviours worth knowing: `traffic-deny` edges are **not**
followed by default (a deny states traffic does not flow, so traversing it
would invent forbidden routes), and an empty result says plainly that absence
of a path is not proof of isolation — the graph is inferred, so its silence is
weaker than a negative. Full details in
[docs/configuration.md](./docs/configuration.md#auditor-reach).

## Drift detection

`auditor diff` compares two audit snapshots (`audit -o json`, array or
NDJSON) and reports what was added, removed, or changed — per-field,
including individual tags:

```bash
auditor audit -o json > monday.json
# ... a week passes ...
auditor audit -o json > friday.json

auditor diff monday.json friday.json                  # human table
auditor diff -o markdown monday.json friday.json      # paste into a PR / issue
auditor diff -o json monday.json friday.json | jq .summary

# CI gate: exit 1 when anything drifted (mirrors `git diff --exit-code`).
auditor diff --exit-code monday.json friday.json
```

Identity is `(provider, id)`; `Raw` and `CreatedAt` are deliberately
excluded from comparison (opt-in noise / immutable-ish).

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

It can also gate on **drift**: pass `baseline` (a previous snapshot) and
the action runs `auditor diff` against the fresh output, appends a
markdown report to the job's step summary, and exposes `drift` /
`drift-summary` outputs. A missing baseline file is warn-and-skip, so
the first run never fails. Scheduled-drift example with `actions/cache`
persisting the snapshot between runs:

```yaml
on:
  schedule: [{ cron: "0 6 * * *" }]

jobs:
  drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/cache@v4
        with:
          path: baseline.json
          key: asset-baseline-${{ github.run_id }}
          restore-keys: asset-baseline-     # newest previous run
      - uses: cloud-auditor/cloud-asset-auditor/.github/actions/audit@v0
        env:
          CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
        with:
          output-file: assets.json
          baseline: baseline.json
          fail-on-drift: "true"             # fail the job when assets changed
      - name: Promote snapshot to next run's baseline
        if: always()
        run: cp assets.json baseline.json
```

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
| `just web`          | Rebuild the Next.js UI into `internal/server/webui/` (**commit the result**) |
| `just web-dev`      | Frontend hot-reload dev server (run `auditor serve` alongside it) |
| `just web-verify`   | Fail if the embedded UI is stale vs `web/` — what CI runs        |

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
| 2 — Cloudflare provider     | shipped  | Accounts, Zones, DNS, R2, KV, Workers, D1, Pages, Access apps, Tunnels, certificate packs / custom / mTLS certificates, Rulesets (account + zone), Page Rules, Load Balancers — every planned resource type implemented |
| 3 — OCI provider            | shipped  | Compartment recursion + region resolution + Compute, Load Balancers, Block / Boot volumes, VCNs, Subnets, Object Storage, Autonomous DBs, DB Systems, Functions, Container Instances, OKE, Vaults, Policies, Users, Groups, Dynamic Groups |
| 4 — Kubernetes provider     | shipped  | Dynamic-client + discovery — every built-in resource type and every CRD with no per-resource code. `--kube-context`, `--kube-namespace`, `--kube-exclude-namespaces` honored; per-GVR Forbidden tolerated; aggregated-API discovery failures degrade to warnings |
| 5 — Web UI                  | shipped  | Embedded SPA + JSON/SSE API. `auditor serve --addr ... --auth none\|basic\|token`. Three tabs: streamed asset table (filter / sort / facets, CSV/JSON/XLSX/HTML export), interactive force-directed topology diagram, live charts dashboard. Plain JS rather than the planned Alpine.js — keeps the binary fully self-contained |
| 6 — Docker                  | shipped  | Multi-stage build → `gcr.io/distroless/static-debian12:nonroot`. Non-root (UID 65532), reproducible-ish (`-trimpath`, ldflags-injected version), accepts `--platform` for multi-arch. ~75 MB rather than the plan's <30 MB target (cloudflare-go/v4 + oci-go-sdk/v65 + k8s client-go are large) |
| 7 — Helm chart              | shipped  | `deploy/helm/cloud-asset-auditor/` — CronJob (default, optional PVC for persisted output) and Deployment (Service + optional Ingress) modes. BYO credentials Secret (`existingSecret`). Read-only `get,list` ClusterRole (overridable). Example values for both modes |
| 8 — GitHub Actions          | shipped  | `ci.yml` (test + lint + gosec + helm lint + smoke), `release.yml` (goreleaser cross-build + cosign keyless + SBOM), `docker.yml` (multi-arch GHCR push + cosign image sign + Trivy SARIF), reusable `actions/audit` composite |
| 9 — Docs                    | shipped  | [`docs/configuration.md`](./docs/configuration.md), [`docs/providers.md`](./docs/providers.md), [`docs/extending.md`](./docs/extending.md). README install paths cover prebuilt release / `go install` / from-source / Docker / Helm |
| 10 — Network topology       | shipped  | `auditor topology` subcommand → JSON / DOT / Mermaid / **Excalidraw** (LR-layered layout, per-service embedded icons, provider-accented cards, dashed arrows for heuristic edges, deterministic seeds for diff-friendly output). Resolvers: `dnsToTarget` (cross-cloud heuristic), `wafBinding` (CF ruleset / Access app / tunnel / page rule → zone, exact), `lbToGateway` (OCI LB → K8s Service by external IP), `gatewayToService` (Ingress / HTTPRoute → backing Service, exact). `/api/v1/topology` returns JSON by default or any format via `?format=` |

## Docs

- **[`docs/configuration.md`](./docs/configuration.md)** — every flag, env var, and config-file key, with precedence rules and exit codes
- **[`docs/providers.md`](./docs/providers.md)** — per-provider auth setup, minimum permission templates (CF token scopes, OCI policy snippets, K8s ClusterRole YAML), and the per-resource implementation matrix
- **[`docs/extending.md`](./docs/extending.md)** — step-by-step worked example for adding a new provider
- **[`CONTRIBUTING.md`](./CONTRIBUTING.md)** — dev setup, conventions, PR flow
- **[`CLAUDE.md`](./CLAUDE.md)** — architecture notes for contributors (and for future Claude Code sessions)
- **[`init-plan.md`](./init-plan.md)** — original phased implementation spec

## License

No `LICENSE` file is committed yet — all rights reserved until one lands.
