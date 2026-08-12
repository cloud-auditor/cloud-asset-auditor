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
| — | NetBird provider (post-plan) | `internal/providers/netbird/` |
| — | Tailscale provider (post-plan) | `internal/providers/tailscale/` |
| — | GCP provider (post-plan) | `internal/providers/gcp/` |
| — | Demo provider (post-plan) | `internal/providers/demo/` |
| 5 | Web UI + JSON/SSE API | `internal/server/` + `web/` (Next.js source) → `internal/server/webui/` (committed static export, embedded) |
| 6 | Container image | `deploy/docker/Dockerfile`, `.dockerignore` |
| 7 | Helm chart | `deploy/helm/cloud-asset-auditor/` |
| 8 | CI / release | `.github/workflows/{ci,release,docker}.yml`, `.github/actions/audit/action.yml`, `.golangci.yml`, `.goreleaser.yaml`, `.trivyignore` |
| 9 | Docs | `docs/{configuration,providers,extending}.md`, README, this file |
| 10 | Topology graph | `internal/core/edge.go`, `internal/topology/`, `internal/cli/topology.go`, `internal/server/topology.go` |

Four cross-cutting subsystems were added **after** the plan (issues #2–#4) and belong to no phase: `internal/logging/` (slog), `internal/telemetry/` (OpenTelemetry tracing), `internal/metrics/` (Prometheus), and `internal/version/` (ldflags-injected `Version`/`Commit`/`Date`, surfaced by `auditor version`). The first three are detailed under **Observability** in the Architecture section below.

Post-plan additions also include `internal/diff/` + `internal/cli/diff.go` (the `auditor diff` drift-detection command) and `internal/output/html.go` (the `-o html` report renderer).

`internal/store/` (post-plan) is a SQLite-backed local database (`--db`, default `<user-config-dir>/auditor/auditor.db`) serving two concerns: an **audit cache** (`audits`+`assets` tables; `auditor audit --cache` writes a snapshot, `--cache-max-age` serves a fresh one and skips providers — `internal/cli/cache.go`) and an **encrypted secrets vault** (`secrets` table; `auditor secrets set/get/list/rm` — `internal/cli/secrets.go`). **Driver is `modernc.org/sqlite` (pure Go) — NOT `mattn/go-sqlite3`** — because the binary builds `CGO_ENABLED=0` (distroless, goreleaser cross-builds); a cgo driver would break that. Secrets are AES-256-GCM with a scrypt-derived key (passphrase from `AUDITOR_SECRETS_PASSPHRASE`), the secret name bound as GCM AAD; the passphrase is never stored. Vaulted secrets are keyed by the **env var the provider reads** (`NETBIRD_API_TOKEN`, …) and loaded into the process env at startup (`root.go::loadVaultedSecrets`) **only when that var isn't already set** — an explicit env var always wins, so provider factories stay unchanged. The cache-write path buffers the full snapshot (a documented exception to the streaming invariant, like xlsx/html); the cache key is the canonicalized provider set.

The top-level CLI command shapes:

- `auditor audit` — runs providers, streams `Asset`s to `internal/output` renderers (JSON / CSV / XLSX / HTML report). `--cache` / `--cache-max-age` add the SQLite snapshot cache (see `internal/store/` above). XLSX adds `--sheet-by` (none|provider|type|region|account|tag:KEY, or several `+`-joined) to split assets across worksheets — e.g. `--sheet-by tag:compartment_id` for one sheet per OCI compartment, or `--sheet-by region+tag:compartment_id` for one sheet per region/compartment labelled `region (compartment)` — and `--summary` to prepend a Summary worksheet (totals + per-sheet/per-type counts, each per-sheet row hyperlinked). Kubernetes adds `--kube-exclude-helm-secrets` to drop Helm v3 release-state Secrets (`type helm.sh/release.v1`) and `--kube-exclude-events` to skip `Event` objects (core `v1` + `events.k8s.io`) — the latter drops them at **discovery** (the whole GVR is never listed), unlike helm-secrets which is a per-item filter because Helm Secrets are a subset of `Secrets`.
- `auditor serve` — embedded SPA, **four** pages (**Dashboard** stat tiles + arrival sparkline + provider ring + per-provider collection health; **Assets** windowed table with facet popovers, density/column controls and a detail drawer carrying the raw payload; **Topology** force-directed diagram with group hulls, per-type glyphs and a minimap; **Exposure** reachability as grouped attack-path chains), plus a ⌘K command palette, toasts, and a persisted light/dark/system theme — see the **Frontend** section below. `/api/v1/{providers,kube/contexts,audit,audit/export,topology,reach,openapi.yaml}` with optional basic/token auth, plus always-open infra endpoints `/healthz` and `/metrics` (Prometheus) that sit **outside** the `/api/v1` namespace. `GET /api/v1/kube/contexts` lists the server's kubeconfig contexts; `audit`/`audit/export`/`topology` accept a `kube_contexts` query param (`validateKubeContexts` drops names not in the server's kubeconfig — the one per-request provider knob, since it selects an existing cluster, not new creds). `/api/v1/topology` exists in two verbs: GET runs a fresh raw-bearing audit; POST builds the graph from assets in the request body (bare array or `{"assets":[...]}`, 128 MiB cap) — that's what the UI's "From streamed assets" button calls. `/api/v1/reach` mirrors that two-verb shape for reachability queries. Routes are registered through `Server.handle`/`handleFunc`, which record the pattern so `TestOpenAPI_EveryHandlerIsDocumented` can check the **handler→spec** direction (the gap CLAUDE.md used to warn about); `TestOpenAPI_AllRefsResolve` catches dangling `$ref`s that plain YAML validity misses.
- `auditor topology` — runs an audit, builds a derived `Topology = {Nodes, Edges}` via `internal/topology` resolvers, renders to JSON / DOT / Mermaid / **D2** / **GraphML** / **Excalidraw** / **HTML**. `--group-by provider|account|region` wraps nodes in subgraph clusters in the dot/mermaid/d2 renderers (validated by `topology.WithGroupBy`, a functional option on `topology.New`; the server mirrors it via the `group_by` query param). New formats wire in three places that must stay in sync: `render.New`'s switch, `topologyContentType` (server download MIME/filename), and the `openapi.yaml` format enum. D2 (`d2.go`) maps `--group-by` onto native D2 containers; GraphML (`graphml.go`) is flat XML for yEd/Gephi/Cytoscape, carrying provider/account/region/type/status as node `<data>` attrs (so it ignores `--group-by` — group in the tool instead).
- `auditor reach` — **reachability analysis** over the topology graph (`internal/topology/reach.go` + `reach_render.go`, CLI in `internal/cli/reach.go`). `--from X` (what can X reach — walks edges forwards), `--to Y` (what can reach Y — walks **backwards**), both (enumerate simple routes), or `--exposed` (start from public entry points). Selectors are case-insensitive globs matched against **both** id and Name. `--kinds` restricts traversal (`traffic-allow` = policy-only view); `--include-deny` is **off by default** because a deny edge states traffic does *not* flow, so traversing it would manufacture forbidden routes. `-o table|json|<any topology format>` — the graph formats work by collapsing the result's paths back into a sub-topology (`ReachResult.Topology()`), so one renderer set serves both commands. `--exit-code` returns `ErrExposed` → exit 1 for CI gating. Renders as `table` by default, and an **empty result explicitly says "absence of a path is not proof of isolation"** — an inferred graph's silence is much weaker than a negative.
- `auditor diff old.json new.json` — drift detection between two `audit -o json` snapshots (array or NDJSON, sniffed). Identity = (provider, id); compares Name/Type/Region/AccountID/Status/Tags only. `-o table|json|markdown`, `--exit-code` exits 1 on drift via an `ErrDrift` sentinel through `Execute()`'s error mapping. Pure logic + renderers live in `internal/diff` (testable); the cobra glue in `internal/cli/diff.go` takes no `cliState`.
- `auditor secrets set/get/list/rm` — manage the encrypted credentials vault in the `--db` SQLite file (`internal/cli/secrets.go`, see `internal/store/` above). Keyed by the provider's env-var name; loaded into the env at startup so factories pick stored creds up transparently.

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
| `CompartmentsConfigurable`   | `SetCompartments([]string)`                                     | `--oci-compartments` (OCIDs or names; subtree-inclusive filtering)         |
| `KubeConfigurable`           | `SetKubeContext/Namespace/ExcludeNamespaces/ExcludeHelmSecrets/ExcludeEvents` | `--kube-context`, `--kube-namespace`, `--kube-exclude-namespaces`, `--kube-exclude-helm-secrets`, `--kube-exclude-events` |
| `NetbirdConfigurable`        | `SetManagementURL(string)`                                      | `--netbird-management-url` (self-hosted base URL; env `NETBIRD_MANAGEMENT_URL`)            |
| `TailscaleConfigurable`      | `SetTailnet(string)` / `SetAPIBaseURL(string)`                  | `--tailscale-tailnet`, `--tailscale-api-url` (env `TAILSCALE_TAILNET`, `TAILSCALE_API_BASE_URL`) |
| `GCPConfigurable`            | `SetScope(string)`                                             | `--gcp-scope` / `--gcp-project` (Cloud Asset Inventory scope; env `GOOGLE_CLOUD_PROJECT` / `GCP_SCOPE`) |

When adding a new CLI flag that needs to reach providers, extend `providerOptions` + `applyProviderOptions`, declare a Configurable interface in `internal/core/provider.go`, and implement it on the provider(s) that care.

### Per-provider gotchas

- **OCI** — Must recurse compartments from the tenancy root (the canonical OCI mistake). Handled in `internal/providers/oci/compartments.go` via the SDK's `CompartmentIdInSubtree=true`. Auth chain (`auth.go`): instance principal (gated by a 250 ms IMDS probe so laptops don't pay the cost) → resource principal (gated by `OCI_RESOURCE_PRINCIPAL_VERSION` env) → config file → env vars. Resource fan-out is per (region × compartment × resource type). **Region default** (`regions.go::resolveRegions`): no `--oci-regions` flag (or the explicit `all` sentinel) now scans **every subscribed region**; on a subscription-lookup failure it falls back to the home region rather than aborting. **Compartment filter** (`compartments.go::filterCompartments`, applied in `collect.go::run`): `--oci-compartments` narrows the full tree to selected compartments — selectors are OCIDs (`ocid1.` prefix) or case-insensitive names, and matching is **subtree-inclusive** (a selected compartment pulls in its descendants via an upward parent-pointer walk; under-scoping is worse than over-scoping for an audit). Empty = all (unchanged). A selector matching nothing emits a non-fatal error but tenancy-global IAM (users/groups/dynamic-groups, which aren't compartment-scoped) still collects. A `listSubscribed` field on the Provider is a test seam (the identity SDK panics on nil auth, so the default path can't be unit-tested with a live client). **Policies** are region-independent but compartment-scoped, so they run once per compartment outside the region loop (`iam.go`); **Users / Groups / DynamicGroups** are tenancy-root-only and run exactly once. Two collector-specific notes: Object Storage's namespace is resolved once via a `sync.Once` cache on the Provider (`object_storage.go::objectStorageNamespace`) and shared across every bucket collector; block/boot volume listing omits `AvailabilityDomain` (optional in oci-go-sdk v65 — confirmed working against the live API), so one per-compartment call covers all ADs.
- **Demo** — Post-plan, and the only provider **not registered from an `init()`**. `demo.Register()` is called explicitly by `root.go`'s `PersistentPreRunE` when `--demo` (viper: `AUDITOR_DEMO`) is set, so `demo` never appears in `core.Registered()` on a normal run — otherwise every audit would report "provider demo failed to initialize", and fabricated assets would sit one mistyped flag away from a real inventory. `Register` is `sync.Once`-guarded because `core.Register` panics on a duplicate and a test binary runs the root command repeatedly. With `--demo` and no `--provider`, `internal/cli/audit.go::selectProviders` resolves the selection to `demo` **alone** (the single funnel `audit`/`topology`/`reach`/`check` share); `serve --demo` does the same via `server.Config.Providers` (`internal/cli/serve.go::serveProviders`) — a browser request omitting `?providers=` must not blend synthetic assets into a real inventory, because nothing downstream marks which were invented. The fixture (`fixtures.go`, ~590 assets for a fictional "Northwind") is fully deterministic — fixed IDs, a fixed epoch, a seeded xorshift for filler only — and `demo_test.go` asserts `topology.Build` over it yields **at least one edge of every `core.EdgeKind`**; that assertion is the reason the fixture is worth having, so don't weaken it when a resolver changes. `Collect` paces the stream over a total wall-clock budget (`AUDITOR_DEMO_DURATION`, `0` = as fast as possible) rather than a per-asset delay, so the fixture can grow without the demo getting slower. It emits three non-fatal errors on purpose — hence `audit --demo` exits 2, which is correct and documented.
- **Kubernetes** — Dynamic client + discovery (`ServerPreferredResources` → `dynamicClient.Resource(gvr).List`), **not** typed clients. **Multi-context** (`--kube-contexts a,b` or `all`; `contexts.go::resolveContexts`): when >1 context resolves, `Collect` dispatches to `runMulti`, which spins up one **child `*Provider` per context** (each on the unchanged single-cluster path) and fans their channels back via `mergeCluster` (errors tagged `context "name": …`). This deliberately keeps the single-cluster code path — and all its fake-client tests — untouched; the seam tests still set `p.discovery`/`p.dynamic`/`p.clusterID` directly. Context enumeration (the `all` sentinel, the server's picker) lives in the **dependency-light `internal/kubecontext` leaf package** (no `init()`, so importing it doesn't register the provider). `--kube-contexts` overrides `--kube-context`; in-cluster collapses to the single mounted SA. That's what makes CRDs come along for free. `internal/providers/kubernetes/discover.go::filterResources` drops subresources (names containing `/`) and anything whose verb list doesn't include `list`; it also drops the `Event` GVRs (matched by `mapping.go::isEventResource` on group+resource — core `""` / `events.k8s.io`, resource `events`, so a CRD named `events` in another group survives) when `--kube-exclude-events` is set. Per-GVR `Forbidden` / `MethodNotSupported` errors are swallowed silently — they mean the SA can't read that type, which is a permission gap, not a bug. `*discovery.ErrGroupDiscoveryFailed` (a downed aggregated API server) is treated as a warning.

  Two **asset-identity** rules, both found against a live 1,592-asset cluster and both load-bearing because identity is `(provider, id)` everywhere — `auditor diff` keys drift on it, `topology/index.go` buckets `byID`, and the store caches by it:
  - `kubernetes.go::assetID` falls back to `k8s/<apiVersion>/<Kind>[/<ns>]/<name>` when `u.GetUID()` is **empty**. Objects the API server *computes* rather than stores have no UID — `metrics.k8s.io` PodMetrics/NodeMetrics and `ComponentStatus`, which was 65 assets on the cluster this was found on, every one landing on the same empty ID so the last one silently won.
  - `discover.go::filterResources` keeps **only the core-group `events`** GVR. Events are one stored object projected through both `""` and `events.k8s.io`, so listing both collected every event twice under the same UID. The core group is kept because it predates `events.k8s.io` (pre-1.19 clusters only have it) and its `involvedObject` payload is what the ecosystem reads. `--kube-exclude-events` still drops both, and a CRD named `events` in a third group still survives.
- **GCP** — Post-plan provider. **Universal via Cloud Asset Inventory** (`searchAllResources`), not per-service SDKs — one paginated REST call returns every resource type across a `projects/`, `folders/`, or `organizations/` scope (the same "ask the platform" model as Kubernetes discovery). **No vendored SDK** — a hand-rolled REST client (`client.go`); the google-cloud-go asset client pulls gRPC + a large tree for a single GET. Auth is **ADC** via `golang.org/x/oauth2/google` (`FindDefaultCredentials`: `GOOGLE_APPLICATION_CREDENTIALS` SA key → gcloud user creds → metadata/workload-identity); the resolved `*http.Client` is cached behind a `sync.Once` (a pre-set `c.hc` is the test seam — bypasses ADC). **Enablement mirrors Cloudflare's token**: the factory requires a scope from env (`GOOGLE_CLOUD_PROJECT` → `projects/X`, or `GCP_SCOPE` verbatim) and errors → skipped otherwise; `--gcp-scope`/`--gcp-project` (via `GCPConfigurable.SetScope`) override the env scope but don't self-enable (flags apply after the factory runs). **Quota-project gotcha**: gcloud *user* ADC 403s with "API requires a quota project" unless `X-Goog-User-Project` is set — `quotaProject()` derives it from a `projects/X` scope automatically; a folder/org scope with user creds needs `GOOGLE_CLOUD_QUOTA_PROJECT` (service accounts need none). Mapping (`mapping.go`): full resource `name`→ID, `assetType`→Type, `location`→Region, `project` (stripped of `projects/`)→AccountID, `state`→Status, `labels`+extras→Tags. The Cloud Asset API must be enabled and the caller needs `roles/cloudasset.viewer`. **Addresses**: `resourceAddresses` walks the free-form `additionalAttributes` blob and keeps every string `net.ParseIP` accepts, joining them (sorted, for determinism) into an **`ip_addresses`** tag — the same key and format OCI's load balancer uses, so `topology/index.go` joins a DNS record to a GCP address with one shared parser and **without** `--include-raw`. It matches on IP-shape rather than a table of known keys because the keys differ per asset type (`ipAddress` on a ForwardingRule, `natIP` inside an Instance's access configs) and GCP adds asset types continuously, so a key table is stale the day it is written.
- **NetBird** — Post-plan provider for the NetBird (WireGuard mesh / zero-trust) REST Management API. **No vendored SDK** — a hand-rolled stdlib `net/http` client (`client.go`); the official `netbirdio/netbird` module pulls the whole management server + gRPC stack and would bloat the CGO-free static binary for a handful of GETs. Auth is a **Personal Access Token** (`NETBIRD_API_TOKEN`, `nbp_` prefix) sent as `Authorization: Token <PAT>` (NOT `Bearer` — that's the separate IdP-JWT scheme). Base URL defaults to the cloud `https://api.netbird.io`; self-hosted overrides via `NETBIRD_MANAGEMENT_URL` / `--netbird-management-url` (the `NetbirdConfigurable` interface rebuilds the client). List endpoints return **bare JSON arrays — no pagination** (don't add a page loop; only events/access-logs paginate and we don't collect them). Error body is just `{"message":…}`; rely on the HTTP status. The account id (`GET /api/accounts`, `sync.Once`) is stamped onto every asset's `AccountID`. Collectors fan out one-per-resource under `--max-concurrency` (`collect.go`), partial failure non-fatal like the other providers. **Secrets:** the `setup_key.key` and `user.password` fields are deliberately omitted from the Go structs so they can't reach `Asset.Raw` even with `--include-raw` (the list endpoints mask them anyway — belt-and-braces). **Topology:** `netbird.peer` address tags (`ip`/`ipv6`/`connection_ip` → `byIP`, `dns_label`/`hostname` → `byHostname`) are indexed in `topology/index.go`, so the existing `dnsToTarget` resolver joins a DNS record/LB pointing at a peer to that peer (heuristic). Peer tags carry the addresses without `--include-raw`, so the join works on a plain audit snapshot too.
- **Tailscale** — Post-plan provider for the Tailscale v2 REST API. **No vendored SDK** — a hand-rolled stdlib client (`client.go`), same rationale as NetBird: the official module drags in the whole tsnet/wgengine stack for a handful of GETs. Auth is an API access token sent as **`Authorization: Bearer`** (NOT NetBird's `Token` scheme — the two mesh providers differ here, and it's an easy copy-paste bug). Tailnet defaults to the **`"-"` sentinel** ("the token's own tailnet"), which doubles as the `AccountID` — a predictable label beats an extra round-trip to resolve the real name. `tailnetPath` escapes the tailnet into a path segment (`@` survives verbatim; legacy org names are email-shaped). Collectors: devices (`?fields=all` — the subnet-route fields are omitted from the default field set and they're what makes a subnet router visible), users, keys (`?all=true`, so OAuth clients and federated identities are inventoried too), DNS (three endpoints folded into one asset; a partial read still emits), and the ACL policy file. **The `/acl` endpoint 404s on a tailnet with no custom policy — that's "not configured", not an error.** **Secrets:** `machineKey`/`nodeKey` (devices) and `key` (auth keys) have **no struct fields at all**, so `--include-raw` cannot round-trip them into `Asset.Raw` — the omission is structural, not incidental. **Determinism:** the policy file's groups/tagOwners/hosts are Go maps, so `policyAssets` sorts keys before emitting; unsorted output would make every `auditor diff` report phantom drift.
- **Cloudflare** — Token-only auth (`CLOUDFLARE_API_TOKEN`); no legacy email+key path. **"Only getting DNS" is a token-scope symptom, not a bug** (`diagnostics.go`): a `Zone`+`Zone.DNS`-only token makes `GET /accounts` return success with **zero accounts** (not a 403), silently zeroing all 10 account-scoped collectors. `collect.go::run` now pre-resolves the account list once (warming the shared `sync.Once`) and emits `noAccountsHint` when it's empty; 403/`code 10000`/`code 9109` errors get a scope hint via `withScopeHint`. Fix is to broaden the token (see docs/providers.md "Why am I only getting DNS?"). `errgroup` capped by `--max-concurrency` (default 5) fans out per-zone and account-scoped collectors. The account list is fetched once per Provider behind a `sync.Once` (`accounts.go::listAccounts`) and shared by every account-scoped collector; accounts are also emitted as `cloudflare.account` assets. Collector quirks: R2's v4 SDK `Buckets.List` discards the pagination cursor, so `r2.go` pages via `start_after` + lexicographic bucket order; `certificates.go` covers three families (per-zone certificate packs, per-zone custom certs, per-account mTLS certs) and re-lists zones itself, joining per-family errors with `errors.Join`; managed rulesets can surface the same ruleset ID at both account and zone scope (discriminated by the `scope` tag); zone-scoped assets always carry `zone_id`/`zone_name` tags — the topology `wafBinding` resolver joins on `zone_id` and matches types `cloudflare.ruleset`, `cloudflare.access_app`, `cloudflare.tunnel`, `cloudflare.page_rule` exactly.

### Topology resolvers (Phase 10)

`internal/topology/Build([]Asset)` runs nine pluggable resolvers over a shared `index` (assets keyed by ID / Type / IP / hostname). Six answer **"what points at what"** (request paths, below); three answer **"who may talk to whom"** (traffic flow, `traffic.go`). Registration order is priority order — the traffic resolvers run last so a request-path edge wins any dedup tie.

- `dnsToTarget` — DNS records → matched LB/Service by IP or CNAME hostname. **Heuristic** confidence (cross-cloud join). The index now also buckets K8s `status.loadBalancer.ingress[].hostname` (via `kubeExternalHosts`), so a CNAME pointing at a hostname-fronted LB resolves to its Service — previously only `.ip` was indexed.
- `lbToGateway` — OCI LB IPs → K8s Service external IPs. **Heuristic**.
- `gatewayToService` — K8s Ingress / HTTPRoute spec → backing Service. **Exact**. Requires `Asset.Raw` (the topology CLI forces `--include-raw=true`).
- `serviceToWorkload` — K8s Service `spec.selector` (Raw) → selected Pods, matched against Pod label **Tags** (`collapseTags` surfaces labels), scoped to the same `AccountID` (cluster) + `namespace`. Fills `core.EdgeKindServiceBackend`. **Exact**. Empty selector (headless/ExternalName) selects nothing — must not match every Pod.
- `ociNetworkContainment` — OCI network backbone via OCID tags joined to `idx.byID` (`core.EdgeKindNetworkContainment`, **Exact**): subnet/{nat,internet,service,local_peering}_gateway/oke.cluster `vcn_id`→VCN, network_load_balancer `subnet_id`→subnet. Driven by the `ociContainmentRules` table — adding a containment edge is one row. (instance→subnet is **not** implementable: the compute collector records no VNIC/subnet OCID.)
- `wafBinding` — CF Rulesets/Access/Page Rules → protected zone. **Exact** (live since the CF collectors shipped — joins `Tags["zone_id"]` to the zone asset; tunnels are in the candidate list but are account-scoped with no `zone_id`, so they never match).

**Traffic-flow resolvers** (`internal/topology/traffic.go`) read policy documents and emit `core.EdgeKindTrafficAllow` / `EdgeKindTrafficDeny` (split by verdict so a renderer can't draw a denial as reachability; `core.TrafficEdgeKind` normalises accept/allow vs drop/deny/reject):

- `kubeNetworkPolicyFlow` — `networking.k8s.io/v1.NetworkPolicy`. Ingress draws `peer pod → policy → selected pod`; egress reverses it. **Exact**, requires `--include-raw`. Note an **empty `spec.podSelector` selects every pod in the namespace** (the default-deny idiom) — unlike a Service selector, it must NOT be skipped. `ipBlock` peers are not expanded (a CIDR is not an asset) and `matchExpressions` selectors are skipped rather than half-honoured (over-matching would invent flows the API server doesn't allow).
- `tailscaleACLFlow` — `tailscale.acl_rule` src/dst selectors → devices (by `acl_tags`), policy groups, users (by `login_name`, case-insensitively), `hosts` aliases, or literal addresses. A trailing `:22` on a dst becomes `Edge.Port` (only a **numeric** final segment counts, so `tag:prod` isn't read as host `tag` on port `prod`).
- `netbirdPolicyFlow` — `netbird.policy_rule` source/destination group ids → peers (via the peer's `group_ids` tag). `bidirectional: true` emits the reverse path explicitly.

**All three route edges *through the rule node* (`source → rule → destination`) rather than drawing the source × destination cross-product.** This is load-bearing, not stylistic: one `allow group:all → group:all` rule over 500 peers is 250,000 direct edges but 1,000 through a rule node, and real tailnets/clusters have exactly those catch-all rules. Wildcard selectors (`*`, `autogroup:*`, an empty NetworkPolicy `from`/`to`) are deliberately **not** expanded for the same reason.

### Detail levels (`internal/topology/collapse.go`)

`Topology.Collapse(level, dim)` produces the high-level network diagram. `--detail low` (default) is the identity; `medium` collapses each group to one node per resource type; `high` collapses each group to a single node. `dim` is the `--group-by` value (defaulting to `provider`). Collapsed nodes get `Type == topology.group` and `member_count` / `member_types` tags; collapsed edges carry `Edge.Count` (rendered as `×N`) and degrade to `heuristic` if **any** constituent was. Edges falling inside one bucket become the node's `internal_edges` tag rather than self-loops. **Call it last** — `DropOrphans` and `FilterByHostname` are statements about individual assets, so they must run while those still exist. `topology.RenderGroupBy(detail, groupBy)` suppresses clustering at `high` (the collapse already made one node per group; clustering too would box each node alone).

Renderer outputs are **deterministic** (sorted nodes/edges + groups, FNV-hashed Excalidraw element IDs) so two runs of the same topology produce byte-identical files. Tests assert this for Excalidraw, D2, and GraphML.

The **Excalidraw renderer** (`excalidraw.go`) emits each node as a 3-element *card* — a provider-accented rounded rectangle, a container-bound text label (name + short Type), and an `image` glyph — all sharing one `groupId` so they drag together. Icons are **original hand-drawn line SVGs** defined in `excalidraw_icons.go` (`iconSet`), categorised by asset Type via `iconKeyForType` (not provider — a database reads as a database on any cloud), base64-encoded into the document's `files` map keyed by a content-stable `fileID`. **Do not vendor a brand icon pack** — same self-contained-binary rule as the web UI (mistake 4); the glyphs are drawn in-repo to avoid trademark/licensing baggage. Determinism caveat: file-entry `created`/`lastRetrieved` are hard-coded `0` (real timestamps would break byte-identical output). The renderer's `Render` and `buildExcalidrawElements` return `(elements, files)`; the per-node element count is **3, not 2** — the structure tests count `rectangle`/`text`/`arrow` exactly (one each per node/edge) and `image` separately, so don't add a second text/rect per node.

### Frontend (`web/` → `internal/server/webui/`)

The UI is a hand-built design system called **Mission Control** — dark-first, one indigo→cyan accent, hairline structure, tabular data type. Everything is hand-rolled: charts, force layout, table windowing, popovers, focus traps, fuzzy match, icons. **Adding an npm dependency is the thing not to do here** (mistake 4); `package.json` has exactly `next`/`react`/`react-dom`.

- **`web/app/globals.css` is the contract** (~1,900 lines). ~76 custom properties and the whole shared class vocabulary live there, and every component styles against it — never hard-code a colour in a component. Page-specific rules live in a stylesheet colocated with the page (`app/assets/assets.css`, …) and imported from it; promote to `globals.css` only when a second surface mounts the same component.
- **Theming**: `:root` is dark. Light comes from `:root[data-theme='light']` *and* `@media (prefers-color-scheme: light) { :root:not([data-theme]) }`, with `[data-theme='dark']` re-asserted so a stored choice beats the OS. `lib/theme.ts::THEME_INIT_SCRIPT` runs inline in `<head>` before first paint (no flash); `layout.tsx` therefore needs `suppressHydrationWarning`. **`--accent` is tuned to be readable as text on `--surface`, which forbids white text on an `--accent` fill — a solid accent background must use `color: var(--accent-ink)`.**
- **URL params drive the app**, which is what makes it screenshot-able, kiosk-able, and linkable: `?run=1` (+`&providers=`/`&kube_contexts=`) starts an audit on load, `?theme=light|dark` pins appearance, `?build=1` renders the topology graph, `?trace=1` runs the reachability query, `?focus=<id>` selects a node, `?from=`/`?to=`/`?q=` prefill Exposure and Assets. The `build`/`trace` ones deliberately wait for the run to **finish** — a graph or an exposure answer computed over a partial inventory is not an early version of the real one, it is a different and quietly wrong one.
- **Count-up animations clamp progress at BOTH ends.** A rAF callback receives the frame's *start* time, which can predate the `performance.now()` sampled when the effect ran, so `(now - t0)` is negative on the first frame more often than it looks. Unclamped, the cubic ease turns that into a huge negative multiplier and the tile paints a nonsense number — which then becomes the next animation's starting value. This shipped and was caught in a screenshot; don't reintroduce it (`Nav.tsx::Ticker`, `StatTile.tsx::useCountUp`).
- **Icons** (`lib/icons.tsx`) mirror `internal/topology/excalidraw_icons.go` — same categories, same drawings — so a node in the browser and the same node in an exported `.excalidraw` read as the same thing. Change one, change both.
- `just demo` (`serve --demo --include-raw`) is the fastest loop for a UI change: no credentials, deterministic data, every edge kind present.
- Screenshots: `chrome --headless --screenshot` fires on the load event and `--timeout` only caps the run, so neither waits for an SSE audit. Drive Chrome over CDP instead (Node 22+ has a built-in `WebSocket`, so it needs no dependencies).

### Cross-cutting invariants (`init-plan.md §6`)

Apply to every commit, every provider, every renderer:

1. **Stream end-to-end.** Never buffer the full asset list. (Two documented exceptions: the XLSX renderer — an `.xlsx` ZIP is finalized at close and its sheets/columns depend on the full set — and the HTML report renderer, whose charts/counts need the full set. JSON/CSV stay streaming.)
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
| `just web`                      | Rebuild the Next.js export into `internal/server/webui/` (**commit the result**) |
| `just web-dev` / `just web-check` | Frontend hot-reload dev server / `tsc --noEmit`                            |
| `just web-verify`               | Fail if `internal/server/webui/` is stale vs `web/` (what the CI `web` job runs) |
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
4. **The frontend must stay a self-contained static export.** The web UI is a Next.js app (`web/`) built with `output: 'export'` into `internal/server/webui/`, which is **committed** and `go:embed`-ed — so `go build`, CI, goreleaser, and the Docker image never need Node. Rules that keep this working, all asserted by `internal/server/webui_test.go`:
   - Run **`just web`** after any change under `web/` and commit the regenerated tree. `just web-verify` (and the `web` CI job) rebuild and diff, so a stale bundle can't merge.
   - Keep **`trailingSlash: true`** in `next.config.mjs`. It makes the export emit `out/assets/index.html` instead of `out/assets.html`; Go's `FileServer` 404s the extensionless form, so flipping it breaks every route but `/`.
   - Keep **`//go:embed all:webui`** — without `all:`, embed skips `_`-prefixed names, and every hashed chunk lives under `_next/`.
   - Pin dependencies **exactly** (the build output is committed, so it must be reproducible) and install with **`--omit=optional`** (skips `sharp`, an unused native dep with open CVEs).
   - Keep **`generateBuildId: () => 'auditor'`** in `next.config.mjs`. Next defaults to a *random* build id per build, which lands in the output path and every page's inlined payload — so two builds of identical source differ and the CI diff fails on every run, making the staleness check meaningless. A constant is safe: chunk filenames already carry content hashes and index.html is served `no-cache`.
   - **Do not add a graph library** (Cytoscape / d3-force / React Flow). The in-browser diagram is a hand-rolled force simulation (`web/lib/layout.ts` + `web/components/GraphCanvas.tsx`); past a few hundred nodes the answer is `--detail medium|high` or a GraphML export into yEd/Gephi, not a bigger layout engine.
5. **Do not add new top-level fields to `core.Asset`.** Put provider-specific richness in `Asset.Raw` (opt-in via `--include-raw`). Adding fields breaks the renderer's golden files and the JSON API contract.
6. **Do not re-pin the Trivy GitHub Action in `docker.yml`.** Trivy is installed from its upstream `install.sh` script on purpose (commit `abebd60`) — a pinned marketplace action drifts out of sync with the Go toolchain the same way `golangci-lint-action` and the gosec image did (mistakes 2 above).
7. **Keep `internal/server/openapi.yaml` in sync with `routes()`.** The spec is hand-maintained, but the sync is now checked in **both** directions: `TestOpenAPI_EveryDocumentedPathHasAHandler` (documented→handler) and `TestOpenAPI_EveryHandlerIsDocumented` (handler→documented, via the patterns `Server.handle` records). Register routes through `s.handle` / `s.handleFunc`, never `s.mux.Handle` directly, or the new check silently stops seeing them. And don't auth-gate `/metrics`, `/healthz`, or `/api/v1/openapi.yaml` — they're deliberately exempt.
8. **Do not let a provider default to "every registered provider" when a synthetic one is in play.** `serve --demo` scopes `server.Config.Providers` to `demo`, and `selectProviders` (both the CLI's and the server's) honours that before falling back to `core.Registered()`. Without it, a browser request that omitted `?providers=` on a credentialed host would blend fabricated demo assets into a real inventory — and nothing downstream of that point marks which assets were invented.
9. **Do not swap `modernc.org/sqlite` for `mattn/go-sqlite3`** (or any cgo SQLite driver) in `internal/store/`. The binary builds `CGO_ENABLED=0` (distroless image, goreleaser cross-builds across linux/darwin/windows × amd64/arm64); a cgo driver needs a C toolchain per target and breaks the static build. `modernc.org/sqlite` is pure Go — keep it.

## Deviations from the plan

Each was a deliberate choice; the rationale matters when revisiting:

- **Phase 2 SDK**: `cloudflare-go/v4` instead of plan's `v2` (`v2` was an early-access generated SDK that's been superseded).
- **Phase 5 frontend**: Next.js (App Router, TypeScript) in **static-export** mode — not Alpine.js, and no longer the hand-written vanilla JS that preceded it. Source lives in `web/` at the repo root (as `init-plan.md` originally specified); the *export* is committed to `internal/server/webui/` and embedded, so the binary stays self-contained with no Node runtime. Three pages: Dashboard (inventory shape), Assets (SSE-streamed table with facets/search/export), Topology (interactive diagram with detail levels, grouping, and edge-kind filters). Audit state lives in `AuditProvider` above the router so navigating between pages doesn't discard a completed audit and re-hit every provider API. Only dependencies are `next`/`react`/`react-dom`; styling is hand-written CSS custom properties (`web/app/globals.css`), no utility framework.
- **Phase 6 image size**: ~75 MB, not the plan's `<30 MB` target. Cloudflare v4 + OCI v65 (70+ service packages) + k8s client-go push the static binary to ~73 MB before distroless adds ~2 MB. Hitting <30 MB would require ripping out provider SDKs or a build-tag pruning scheme that doesn't exist upstream.
- **Phase 10 UI**: no Cytoscape.js; the Topology page is a hand-rolled force-directed SVG viewer (`web/components/GraphCanvas.tsx` + `web/lib/layout.ts` — deterministic physics seeded from a hash of the node key, pan/zoom/drag, inspector panel), alongside CLI + JSON API renderers (JSON / DOT / Mermaid / Excalidraw). The Excalidraw export is the practical "editable canvas" — pipe `auditor topology -o excalidraw > topology.excalidraw`, drop into excalidraw.com or the desktop app, edit by hand. Each node is an icon-bearing card (see the Excalidraw renderer note above); arrows are bound to nodes so rearranging keeps them attached.

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
| `internal/core`                      | 100% |
| `internal/filter`                    | ~96% |
| `internal/diff`                      | ~95% |
| `internal/providers/demo`            | ~95% |
| `internal/logging`                   | ~95% |
| `internal/policy`                    | ~94% |
| `internal/config`                    | ~93% |
| `internal/providers/kubernetes`      | ~92% |
| `internal/topology`                  | ~91% |
| `internal/kubecontext`               | ~90% |
| `internal/output`                    | ~89% |
| `internal/providers/tailscale`       | ~85% |
| `internal/server`                    | ~83% |
| `internal/providers/gcp`             | ~81% |
| `internal/telemetry`                 | ~78% |
| `internal/store`                     | ~77% |
| `internal/providers/netbird`         | ~75% |
| `internal/metrics`                   | ~75% |
| `internal/cli`                       | ~51% |
| `internal/providers/cloudflare`      | ~35% |
| `internal/providers/oci`             | ~23% |
| `internal/version`                   | 0% (ldflags only, no tests) |

Provider coverage is intentionally lower because most of the code is SDK glue; the mapping bits are well-covered, the network bits wait for integration tests.

## CI gates

CI runs six jobs on every PR — `ci.yml`:

1. **test** — `go test -race -cover ./...`
2. **web** — rebuilds the Next.js export and diffs it against `internal/server/webui/`, so a frontend change that forgot `just web` can't merge with a stale bundle
3. **lint** — `golangci-lint run` (v2, installed from source — see above)
4. **security** — `gosec` (pinned `v2.21.4`)
5. **helm** — `helm lint` in both example-values modes + `helm template`
6. **smoke** — build + `auditor --help`, `version`, `providers`, and the Phase 1 exit-criterion `audit --provider none -o json == []`

Release flow:

- Push a `v*` tag → `release.yml` runs `goreleaser` (cross-builds, cosign keyless, SBOM, GitHub Release).
- Push to `main` + tags → `docker.yml` builds multi-arch (`linux/amd64`, `linux/arm64`), pushes to GHCR, cosign-signs each tag by digest, then runs a Trivy scan with a HIGH/CRITICAL gate (Trivy installed from upstream `install.sh`, not a pinned action — see mistake 6).
- The reusable composite at `.github/actions/audit/action.yml` lets other repos run the auditor in one step.
