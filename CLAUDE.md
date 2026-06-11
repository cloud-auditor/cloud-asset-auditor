# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**All 10 phases of [`init-plan.md`](./init-plan.md) shipped and every provider's resource catalog is fully implemented; the project is in maintenance / enhancement mode.**

- Cloudflare: **all resource types implemented** — the former `stubs.go` is gone; collectors live one-per-file (`r2.go`, `kv.go`, `workers.go`, `d1.go`, `pages.go`, `access.go`, `tunnels.go`, `certificates.go`, `rulesets.go`, `page_rules.go`, `load_balancers.go`, `accounts.go`) alongside the original `zones.go` / `dns.go`.
- OCI: **all resource types implemented** — the former `stubs.go` is gone; collectors live in `network.go`, `storage.go`, `object_storage.go`, `database.go`, `functions.go`, `container_instances.go`, `oke.go`, `vaults.go`, `iam.go` alongside the original `compute.go` / `load_balancer.go`.
- (Kubernetes is universal via dynamic-client + discovery — no stubs)

Anything else substantial — read [`init-plan.md`](./init-plan.md) end-to-end first. It is still the single source of truth for layout, abstractions, and phase ordering. Document any deviation explicitly (see the **Deviations from the plan** section below).

For user-facing dev workflow (PRs, commit conventions, issue reporting), defer to [`CONTRIBUTING.md`](./CONTRIBUTING.md). This file is *operational knowledge for the next Claude session*, not duplicated dev docs.

## Where things live

Each phase from the plan lives in one place:

| Phase | Concern | Files |
| ----- | ------- | ----- |
| 1 | Foundation | `internal/core/`, `internal/output/`, `internal/cli/`, `internal/config/`, `cmd/auditor/main.go`, `justfile` |
| 2 | Cloudflare provider | `internal/providers/cloudflare/` |
| 3 | OCI provider | `internal/providers/oci/` |
| 4 | Kubernetes provider | `internal/providers/kubernetes/` |
| 5 | Web UI + JSON/SSE API | `internal/server/` + `internal/server/web/` (embedded HTML/CSS/JS) |
| 6 | Container image | `deploy/docker/Dockerfile`, `.dockerignore` |
| 7 | Helm chart | `deploy/helm/cloud-asset-auditor/` |
| 8 | CI / release | `.github/workflows/{ci,release,docker}.yml`, `.github/actions/audit/action.yml`, `.golangci.yml`, `.goreleaser.yaml`, `.trivyignore` |
| 9 | Docs | `docs/{configuration,providers,extending}.md`, README, this file |
| 10 | Topology graph | `internal/core/edge.go`, `internal/topology/`, `internal/cli/topology.go`, `internal/server/topology.go` |

Four cross-cutting subsystems were added **after** the plan (issues #2–#4) and belong to no phase: `internal/logging/` (slog), `internal/telemetry/` (OpenTelemetry tracing), `internal/metrics/` (Prometheus), and `internal/version/` (ldflags-injected `Version`/`Commit`/`Date`, surfaced by `auditor version`). The first three are detailed under **Observability** in the Architecture section below.

The three top-level CLI command shapes:

- `auditor audit` — runs providers, streams `Asset`s to `internal/output` renderers (JSON / CSV / XLSX). XLSX adds `--sheet-by` (none|provider|type|region|account|tag:KEY, or several `+`-joined) to split assets across worksheets — e.g. `--sheet-by tag:compartment_id` for one sheet per OCI compartment, or `--sheet-by region+tag:compartment_id` for one sheet per region/compartment labelled `region (compartment)` — and `--summary` to prepend a Summary worksheet (totals + per-sheet/per-type counts, each per-sheet row hyperlinked). Kubernetes adds `--kube-exclude-helm-secrets` to drop Helm v3 release-state Secrets (`type helm.sh/release.v1`).
- `auditor serve` — embedded SPA + `/api/v1/{providers,audit,audit/export,topology,openapi.yaml}` with optional basic/token auth, plus always-open infra endpoints `/healthz` and `/metrics` (Prometheus) that sit **outside** the `/api/v1` namespace.
- `auditor topology` — runs an audit, builds a derived `Topology = {Nodes, Edges}` via `internal/topology` resolvers, renders to JSON / DOT / Mermaid / **Excalidraw**.

## Architecture (from `init-plan.md` §2)

The design rests on three small contracts. **Don't extend them without good reason** — every provider, renderer, and topology resolver depends on their shape.

- **`core.Asset`** — the canonical, intentionally minimal struct. Provider-specific richness lives in opt-in `Raw json.RawMessage` (gated on `--include-raw`). Resist adding new top-level fields.
- **`core.Provider`** — `Name()` + `Validate(ctx)` + `Collect(ctx) (<-chan Asset, <-chan error)`. Channels are mandatory: streaming keeps memory bounded against large K8s clusters (50k+ objects) and lets the UI render rows as they arrive. Both channels MUST close exactly once; errors are non-fatal (push to `errs` and keep going).
- **`output.Renderer`** — consumes the asset channel and writes to an `io.Writer`. JSON (array or NDJSON via `--stream`), CSV (flattens `Tags` into one column), and XLSX (`internal/output/xlsx.go`, via `github.com/xuri/excelize/v2`). XLSX is the **one renderer that buffers the whole stream** — an `.xlsx` is a ZIP finalized at close, and sheets/columns aren't known until every asset is seen. It partitions assets into worksheets by `SheetBy` — one dimension or several `+`-joined into a composite (`region+tag:compartment_id` → a sheet per region/compartment, labelled `head (rest / …)`) — expands `Tags` into one column per key (per-sheet union, minus any tag used as a grouping dimension; a tag header that would collide with a core column — e.g. a `name` label vs the core `Name` — is disambiguated to `Name (tag)` via `tagHeaderNames`), resolves group values that match an asset ID to that asset's Name (so `tag:compartment_id` → compartment names), and co-locates a "container" asset (the compartment itself) into its children's group. Parsing/validation of `SheetBy` is centralised in `parseDimensions`. `Summary: true` (`--summary`) prepends a "Summary" sheet — total, per-sheet counts (each row hyperlinked to its sheet by its final sanitized name), and per-type counts — built in `writeSummarySheet` after sheet names are reserved up front so the links resolve.

Providers register themselves into a `registry` map (via package `init()`). New providers are wired into the binary by adding a blank import to `cmd/auditor/main.go` — the only outside touch point.

**Optional Configurable interfaces** in `internal/core/provider.go` let the CLI push knob values without changing the base contract. Each is type-asserted in `internal/cli/audit.go::applyProviderOptions` and silently skipped when not implemented:

| Interface                    | Method                                                          | Flag                                                                       |
| ---------------------------- | --------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `ConcurrencyConfigurable`    | `SetMaxConcurrency(int)`                                        | `--max-concurrency`                                                        |
| `IncludeRawConfigurable`     | `SetIncludeRaw(bool)`                                           | `--include-raw`                                                            |
| `ProfileConfigurable`        | `SetProfile(string)`                                            | `--oci-profile`                                                            |
| `RegionsConfigurable`        | `SetRegions([]string)`                                          | `--oci-regions` (the `"all"` sentinel is the provider's responsibility)    |
| `KubeConfigurable`           | `SetKubeContext/Namespace/ExcludeNamespaces/ExcludeHelmSecrets` | `--kube-context`, `--kube-namespace`, `--kube-exclude-namespaces`, `--kube-exclude-helm-secrets` |

When adding a new CLI flag that needs to reach providers, extend `providerOptions` + `applyProviderOptions`, declare a Configurable interface in `internal/core/provider.go`, and implement it on the provider(s) that care.

### Per-provider gotchas

- **OCI** — Must recurse compartments from the tenancy root (the canonical OCI mistake). Handled in `internal/providers/oci/compartments.go` via the SDK's `CompartmentIdInSubtree=true`. Auth chain (`auth.go`): instance principal (gated by a 250 ms IMDS probe so laptops don't pay the cost) → resource principal (gated by `OCI_RESOURCE_PRINCIPAL_VERSION` env) → config file → env vars. Resource fan-out is per (region × compartment × resource type). **Region default** (`regions.go::resolveRegions`): no `--oci-regions` flag (or the explicit `all` sentinel) now scans **every subscribed region**; on a subscription-lookup failure it falls back to the home region rather than aborting. A `listSubscribed` field on the Provider is a test seam (the identity SDK panics on nil auth, so the default path can't be unit-tested with a live client). **Policies** are region-independent but compartment-scoped, so they run once per compartment outside the region loop (`iam.go`); **Users / Groups / DynamicGroups** are tenancy-root-only and run exactly once. Two collector-specific notes: Object Storage's namespace is resolved once via a `sync.Once` cache on the Provider (`object_storage.go::objectStorageNamespace`) and shared across every bucket collector; block/boot volume listing omits `AvailabilityDomain` (optional in oci-go-sdk v65 — confirmed working against the live API), so one per-compartment call covers all ADs.
- **Kubernetes** — Dynamic client + discovery (`ServerPreferredResources` → `dynamicClient.Resource(gvr).List`), **not** typed clients. That's what makes CRDs come along for free. `internal/providers/kubernetes/discover.go::filterResources` drops subresources (names containing `/`) and anything whose verb list doesn't include `list`. Per-GVR `Forbidden` / `MethodNotSupported` errors are swallowed silently — they mean the SA can't read that type, which is a permission gap, not a bug. `*discovery.ErrGroupDiscoveryFailed` (a downed aggregated API server) is treated as a warning.
- **Cloudflare** — Token-only auth (`CLOUDFLARE_API_TOKEN`); no legacy email+key path. `errgroup` capped by `--max-concurrency` (default 5) fans out per-zone and account-scoped collectors. The account list is fetched once per Provider behind a `sync.Once` (`accounts.go::listAccounts`) and shared by every account-scoped collector; accounts are also emitted as `cloudflare.account` assets. Collector quirks: R2's v4 SDK `Buckets.List` discards the pagination cursor, so `r2.go` pages via `start_after` + lexicographic bucket order; `certificates.go` covers three families (per-zone certificate packs, per-zone custom certs, per-account mTLS certs) and re-lists zones itself, joining per-family errors with `errors.Join`; managed rulesets can surface the same ruleset ID at both account and zone scope (discriminated by the `scope` tag); zone-scoped assets always carry `zone_id`/`zone_name` tags — the topology `wafBinding` resolver joins on `zone_id` and matches types `cloudflare.ruleset`, `cloudflare.access_app`, `cloudflare.tunnel`, `cloudflare.page_rule` exactly.

### Topology resolvers (Phase 10)

`internal/topology/Build([]Asset)` runs four pluggable resolvers over a shared `index` (assets keyed by ID / Type / IP / hostname):

- `dnsToTarget` — DNS records → matched LB/Service by IP or CNAME. **Heuristic** confidence (cross-cloud join).
- `lbToGateway` — OCI LB IPs → K8s Service external IPs. **Heuristic**.
- `gatewayToService` — K8s Ingress / HTTPRoute spec → backing Service. **Exact**. Requires `Asset.Raw` (the topology CLI forces `--include-raw=true`).
- `wafBinding` — CF Rulesets/Access/Tunnels/Page Rules → protected zone. **Exact** (live since the CF collectors shipped — joins `Tags["zone_id"]` to the zone asset).

Renderer outputs are **deterministic** (sorted nodes/edges, FNV-hashed Excalidraw element IDs) so two runs of the same topology produce byte-identical files. Tests assert this for Excalidraw.

### Cross-cutting invariants (`init-plan.md §6`)

Apply to every commit, every provider, every renderer:

1. **Stream end-to-end.** Never buffer the full asset list. (Sole documented exception: the XLSX renderer, which *must* buffer — an `.xlsx` ZIP is finalized at close and its sheets/columns depend on the full set. JSON/CSV stay streaming.)
2. **Plumb `context.Context` through every SDK call.** Ctrl+C must stop work in <1 s.
3. **`log/slog` to stderr only.** stdout is reserved for renderer output when `--output-file` is unset.
4. **Never log secrets.** Use a redaction helper at every error-wrapping site.
5. **Partial failure is normal.** If OCI times out, still emit Cloudflare results; map "some assets, some errors" to exit code 2.
6. **Version the web API.** `/api/v1/audit`, not `/api/audit`.

### Observability (post-plan: logging / tracing / metrics / OpenAPI)

Added after the plan (issues #2–#4). Logging + tracing are installed in `internal/cli/root.go::PersistentPreRunE`; metrics + the OpenAPI spec are served from `internal/server/server.go::routes`. Three **persistent root flags** drive them — `--log-level`, `--log-format`, `--tracing` — all viper-bound, so `AUDITOR_LOG_LEVEL` / `AUDITOR_LOG_FORMAT` / `AUDITOR_TRACING` env vars and config-file keys work too.

- **Logging** (`internal/logging/`) — structured `slog`, installed as the process default at startup so package-level `slog.*` calls and injected loggers share one config. `--log-level` (debug|info|warn|error, default info; an invalid level **fails startup**), `--log-format` (text|json, default text; an unknown format **silently falls back to text**). stderr only (invariant 3).
- **Tracing** (`internal/telemetry/`) — OpenTelemetry, opt-in `--tracing` (off|stdout|otlp, **default off** = noop provider, zero overhead). `stdout` mode writes spans to **stderr**, not stdout (stdout is renderer-reserved). `otlp` endpoint precedence: explicit flag → `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` → `OTEL_EXPORTER_OTLP_ENDPOINT`. Audits (CLI and server) emit a parent `audit` span with child `provider.collect` spans; the server also wraps every handler in `otelhttp` (filtering `/healthz`). `Setup()` is idempotent; `Shutdown()` is flushed on CLI exit (5 s) and server shutdown (10 s).
- **Metrics** (`internal/metrics/`) — Prometheus on a **project-scoped registry** (not the global default; only `process_*`/`go_*` are pulled in). Served at **`GET /metrics`**, always open and **auth-exempt** (scraper semantics, like `/healthz`). No flags. Recorded during collect in both `internal/cli/audit.go::forward` and `internal/server/audit.go::forward`: `auditor_assets_collected_total{provider,type}`, `auditor_audit_errors_total{provider}`, `auditor_audit_duration_seconds{provider}` (histogram, 0.1 s–600 s buckets), and `auditor_server_sse_clients` (gauge, web-UI only). Helm `ServiceMonitor` is opt-in (`mode=deployment` + `monitoring.serviceMonitor.enabled`, default off).
- **OpenAPI** (`internal/server/openapi.yaml`) — OpenAPI 3.1 spec, `//go:embed`-ed (`embed.go`) and served verbatim at **`GET /api/v1/openapi.yaml`** — the only `/api/*` path that is **auth-exempt**. **Hand-maintained:** keep it in sync with `routes()` when you add/change an endpoint. `TestOpenAPI_EveryDocumentedPathHasAHandler` enforces documented→handler, but **not** the reverse (a new handler missing from the spec won't fail CI).

## Common operations

Project uses **`just`** (not `make`). Run `just` with no args to list every recipe.

| Recipe                          | What it does                                                                  |
| ------------------------------- | ----------------------------------------------------------------------------- |
| `just build`                    | Builds `./bin/auditor` with ldflags-injected version/commit/date              |
| `just test`                     | `go test -race -cover ./...`                                                  |
| `just test-update`              | Regenerates renderer golden files                                             |
| `just lint`                     | `golangci-lint run` (requires v2.x — see below)                               |
| `just tidy`                     | `go mod tidy` (regenerates go.sum)                                            |
| `just smoke`                    | Build + verify Phase 1 exit criterion (`audit --provider none -o json == []`) |
| `just docker` / `just docker-run` | Container build + run                                                       |
| `just helm-lint` / `just helm-template` | Helm chart validation                                                 |

Run a single test: `go test -run TestBuild_LBToK8sService ./internal/topology/...`

### Linting locally

The `golangci/golangci-lint-action@v6 with version: latest` action resolves to the **v1.x** line, which was built with Go 1.24 and refuses to lint code targeting Go 1.26+ (CI hit this; commit `650d572`). The fix — installed automatically by `ci.yml` and what you should do locally — is to build v2 from source against the project toolchain:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
golangci-lint run ./...
```

Config in [`.golangci.yml`](./.golangci.yml) uses the **v2 schema** (`version: "2"`, `linters.default: none`, formatters split into their own block). Don't migrate it back to v1.

## Don't repeat these mistakes

Things that broke at some point and have lasting "don't undo this" notes — preserve them.

1. **Do not call `viper.SetConfigType("yaml")` in `internal/config/config.go`.** Viper's `searchInPath` has a special branch when `configType` is set: it also matches the **extensionless** filename. CI builds the binary as `./auditor` in the workspace root, which then matches; viper tries to parse the ELF bytes as YAML and explodes ("yaml: control characters are not allowed"). Caught in commit `be5350f`; regression test in `internal/config/config_test.go::TestInit_IgnoresExtensionlessAuditorFile`.
2. **Do not pin `golangci-lint-action@v6 latest`** in CI — see Linting section above. Use `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` instead.
3. **Do not pin `cloudflare-go/v2`** (despite what `init-plan.md §3` Phase 2 says). It's been superseded by `v4`. The v4 API uses `cloudflare.F(value)` to wrap required params and `AutoPager` for iteration.
4. **Do not vendor Cytoscape.js / Alpine.js / any third-party JS** into `internal/server/web/`. The Phase 5 + Phase 10 deviations are deliberate: keep the binary fully self-contained. The Topology page uses DOT/Mermaid/Excalidraw exports instead of an in-browser graph view.
5. **Do not add new top-level fields to `core.Asset`.** Put provider-specific richness in `Asset.Raw` (opt-in via `--include-raw`). Adding fields breaks the renderer's golden files and the JSON API contract.
6. **Do not re-pin the Trivy GitHub Action in `docker.yml`.** Trivy is installed from its upstream `install.sh` script on purpose (commit `abebd60`) — a pinned marketplace action drifts out of sync with the Go toolchain the same way `golangci-lint-action` and the gosec image did (mistakes 2 above).
7. **Keep `internal/server/openapi.yaml` in sync with `routes()`.** The spec is hand-maintained; `TestOpenAPI_EveryDocumentedPathHasAHandler` only checks documented→handler, so a newly added handler that you forget to document will pass CI but leave the spec lying. And don't auth-gate `/metrics`, `/healthz`, or `/api/v1/openapi.yaml` — they're deliberately exempt.

## Deviations from the plan

Each was a deliberate choice; the rationale matters when revisiting:

- **Phase 2 SDK**: `cloudflare-go/v4` instead of plan's `v2` (`v2` was an early-access generated SDK that's been superseded).
- **Phase 5 frontend**: vanilla JS, not Alpine.js (self-contained binary; smaller payload; simpler review surface). Same feature set: SSE streaming, sort, filter, provider/type facets, CSV/JSON export, sticky header. Lives in `internal/server/web/` (plan put `web/` at the repo root; embedded assets are conventionally placed inside the package that uses them in Go).
- **Phase 6 image size**: ~75 MB, not the plan's `<30 MB` target. Cloudflare v4 + OCI v65 (70+ service packages) + k8s client-go push the static binary to ~73 MB before distroless adds ~2 MB. Hitting <30 MB would require ripping out provider SDKs or a build-tag pruning scheme that doesn't exist upstream.
- **Phase 10 UI**: no interactive Cytoscape.js tab; instead, CLI + JSON API with four renderers (JSON / DOT / Mermaid / Excalidraw). The Excalidraw export is the practical "editable canvas" — pipe `auditor topology -o excalidraw > topology.excalidraw`, drop into excalidraw.com or the desktop app, edit by hand. Arrows are bound to nodes so rearranging keeps them attached.

## Testing strategy

What's actually in the repo today (different from init-plan.md §5's targets):

- **Pure mapping tests** per provider — `*ToAsset` functions tested with synthetic SDK structs. No SDK client mocking yet.
- **Renderer golden files** in `internal/output/testdata/` for JSON array, JSON stream (NDJSON), CSV. Regenerate with `just test-update`.
- **Topology resolvers** tested against a canonical synthetic chain (CF DNS → OCI LB → K8s Service + Ingress) in `internal/topology/topology_test.go`. No SDK mocks needed — pure asset literals.
- **Server tests** use `httptest.NewServer` with the real handler chain; SSE wire format parsed by a small in-test reader.
- **Config tests** use `t.Chdir` + `t.Setenv` to isolate the working directory and `$HOME`; they're how the viper bare-filename regression is defended against.

What's missing (open work for future PRs):

- **Integration tests behind `//go:build integration`** were spec'd in §5 but not yet implemented. A nightly workflow against a sandbox tenancy is the right shape.
- **`envtest`-based Kubernetes tests** were planned but not added; the `dynamic/fake.NewSimpleDynamicClientWithCustomListKinds` we use today covers the list-path adequately.

Coverage snapshot (from latest `just test`):

| Package                              | Coverage |
| ------------------------------------ | -------- |
| `internal/logging`                   | ~95%     |
| `internal/core`                      | ~94%     |
| `internal/config`                    | ~93%     |
| `internal/topology`                  | ~89%     |
| `internal/output`                    | ~82%     |
| `internal/telemetry`                 | ~78%     |
| `internal/metrics`                   | ~75%     |
| `internal/server`                    | ~63%     |
| `internal/providers/kubernetes`      | ~47%     |
| `internal/providers/oci`             | ~19%     |
| `internal/providers/cloudflare`      | ~20%     |
| `internal/version`                   | 0% (ldflags only, no tests) |

Provider coverage is intentionally lower because most of the code is SDK glue; the mapping bits are well-covered, the network bits wait for integration tests.

## CI gates

CI runs five jobs on every PR — `ci.yml`:

1. **test** — `go test -race -cover ./...`
2. **lint** — `golangci-lint run` (v2, installed from source — see above)
3. **security** — `gosec` (pinned `v2.21.4`)
4. **helm** — `helm lint` in both example-values modes + `helm template`
5. **smoke** — build + `auditor --help`, `version`, `providers`, and the Phase 1 exit-criterion `audit --provider none -o json == []`

Release flow:

- Push a `v*` tag → `release.yml` runs `goreleaser` (cross-builds, cosign keyless, SBOM, GitHub Release).
- Push to `main` + tags → `docker.yml` builds multi-arch (`linux/amd64`, `linux/arm64`), pushes to GHCR, cosign-signs each tag by digest, then runs a Trivy scan with a HIGH/CRITICAL gate (Trivy installed from upstream `install.sh`, not a pinned action — see mistake 6).
- The reusable composite at `.github/actions/audit/action.yml` lets other repos run the auditor in one step.
