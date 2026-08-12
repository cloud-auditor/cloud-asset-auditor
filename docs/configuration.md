# Configuration reference

Three sources contribute to every config value, in this **precedence order**
(higher wins):

1. **Command-line flag** — e.g. `-o csv`, `--timeout 5m`.
2. **Environment variable** — prefix `AUDITOR_`, dots become underscores,
   dashes become underscores, uppercased. So the flag `--max-concurrency`
   maps to `AUDITOR_MAX_CONCURRENCY`; the config key `output.format` maps
   to `AUDITOR_OUTPUT_FORMAT`.
3. **YAML config file** — `--config <path>` if set, else the first hit
   from `./auditor.yaml` then `~/.config/auditor.yaml`. A missing file
   is silently ignored (not an error).

Provider credentials don't follow `AUDITOR_*`; they use each SDK's native
env-var names (`CLOUDFLARE_API_TOKEN`, `OCI_TENANCY`, `KUBECONFIG`, …) —
see [providers.md](./providers.md) for the per-provider list.

---

## Demo mode (applies to every subcommand)

`--demo` (env `AUDITOR_DEMO=1`, config key `demo: true`) installs a built-in
synthetic provider holding a complete fictional multi-cloud inventory, so
every command works with **no credentials at all**:

```bash
auditor serve --demo                       # the full UI, nothing to configure
auditor topology --demo -o html > topo.html
auditor reach --demo --exposed
AUDITOR_DEMO_DURATION=0 auditor audit --demo -o json   # 0 disables the paced stream
```

Two behaviours worth knowing:

- With `--demo` and no `--provider`, the selection resolves to `demo` **alone**.
  A demo run that also fired every real provider at live credentials would be
  a nasty surprise. An explicit `--provider` still wins, so
  `--demo --provider demo,kubernetes` shows the fixture beside a real cluster.
- `audit --demo` **exits 2**. The fixture reports three simulated non-fatal
  provider errors on purpose — partial failure is the normal case in a real
  audit and the UI has dedicated rendering for it — and three errors is a
  partial failure, which is exit code 2 by design.

See [providers.md](./providers.md#demo-data) for what the fixture contains.

---

## Logging (applies to every subcommand)

Two persistent flags configure the structured logger that ships with
the binary. Logs go to **stderr** only — stdout is reserved for renderer
output (so `auditor audit ... -o json | jq` works regardless of log
verbosity).

| Flag             | Env / config key       | Default | Notes                                       |
| ---------------- | ---------------------- | ------- | ------------------------------------------- |
| `--log-level`    | `AUDITOR_LOG_LEVEL`    | `info`  | `debug` \| `info` \| `warn` \| `error`     |
| `--log-format`   | `AUDITOR_LOG_FORMAT`   | `text`  | `text` for terminals, `json` for log aggregators |

`json` produces one record per line, parseable by anything that speaks
the standard `log/slog` JSON shape (`time`, `level`, `msg`, plus
free-form key/value attributes). Unknown formats fall back to `text`
rather than crashing the binary — a production typo (`JSON`, `yaml`)
shouldn't take the process down.

---

## Tracing (applies to every subcommand)

Optional OpenTelemetry tracing. Off by default — pays zero overhead until
turned on. Every audit run produces a parent `audit` span with one
`provider.collect` child span per provider; the HTTP server emits one
span per request (with `/healthz` filtered out as noise).

| Flag         | Env / config key  | Default | Notes                                            |
| ------------ | ----------------- | ------- | ------------------------------------------------ |
| `--tracing`  | `AUDITOR_TRACING` | `off`   | `off` \| `stdout` \| `otlp`                      |

- **`off`** — `noop` tracer installed; `telemetry.Tracer().Start(...)` is a free no-op everywhere in the code.
- **`stdout`** — pretty-printed span JSON to **stderr** (not stdout, so renderer output stays pipe-friendly). Useful for local dev.
- **`otlp`** — OTLP/HTTP exporter to a collector (Jaeger / Tempo / Grafana Agent / OTel Collector). Honors the standard OTel SDK env vars:
  - `OTEL_EXPORTER_OTLP_ENDPOINT` — e.g. `https://otel.example.com:4318`
  - `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` — overrides the above for traces only
  - `OTEL_EXPORTER_OTLP_HEADERS` — e.g. `Authorization=Bearer ...`
  - Full env-var spec: https://opentelemetry.io/docs/specs/otel/protocol/exporter/

`--tracing=stdout` uses **stderr** (not stdout) intentionally — the
renderer-output discipline that lets `auditor audit -o json | jq` work
applies to tracing output too.

---

## Metrics (`auditor serve`)

`auditor serve` exposes Prometheus metrics at **`GET /metrics`**. Always
open (same semantics as `/healthz` — scrapers don't carry credentials).

| Metric                                                            | Type      | Labels             | Meaning                                                              |
| ----------------------------------------------------------------- | --------- | ------------------ | -------------------------------------------------------------------- |
| `auditor_assets_collected_total`                                  | counter   | `provider`, `type` | One per Asset emitted by a Collect run                               |
| `auditor_audit_duration_seconds`                                  | histogram | `provider`         | Per-provider wall-clock for the full Collect + forward               |
| `auditor_audit_errors_total`                                      | counter   | `provider`         | Per-provider non-nil errors received from the channel                |
| `auditor_server_sse_clients`                                      | gauge     | —                  | Active `/api/v1/audit` SSE subscribers                               |
| `process_*`, `go_*`                                               | mixed     | —                  | Standard process + Go runtime collectors                             |

The same `internal/metrics` package is shared between `auditor audit`
and `auditor serve`, so the counters and histograms also tick during
CLI audits — they're just not exposed anywhere (the CLI process exits
before a scraper could see them).

To scrape from a Prometheus Operator setup, enable the chart's
ServiceMonitor:

```yaml
# in your values.yaml
monitoring:
  serviceMonitor:
    enabled: true
    labels:
      release: kube-prometheus-stack   # match the Operator's selector
```

---

## API contract

The full machine-readable description of `/api/v1/*` lives at
[`internal/server/openapi.yaml`](../internal/server/openapi.yaml) and
is also served by the running server at **`GET /api/v1/openapi.yaml`**
(reachable without auth — client generators don't carry credentials,
and the spec contains no secrets).

Use it with any OpenAPI 3.1 tool:

```bash
# Generate a Go client
oapi-codegen -package auditorclient http://localhost:8080/api/v1/openapi.yaml > client.go

# Browse interactively
docker run --rm -p 8087:8080 \
  -e SWAGGER_JSON_URL=http://host.docker.internal:8080/api/v1/openapi.yaml \
  swaggerapi/swagger-ui

# Lint after editing
redocly lint internal/server/openapi.yaml
```

The Go test suite validates the spec structurally on every PR
(`internal/server/openapi_test.go`) and asserts every documented path
has a registered handler — adding a new endpoint without a matching
spec entry (or vice versa) fails CI.

---

## `auditor audit`

Collect assets from one or more providers and render them as JSON, CSV, XLSX, or a self-contained HTML report.

| Flag                              | Env / config key                     | Default       | Notes |
| --------------------------------- | ------------------------------------ | ------------- | ----- |
| `--provider strings`              | `AUDITOR_PROVIDER`                   | (all)         | Comma-separated. Use the literal `none` to run zero providers. |
| `-o`, `--output string`           | `AUDITOR_OUTPUT`                     | `json`        | `json`, `csv`, `xlsx`, or `html` (one self-contained report page: summary cards, SVG charts, sortable/filterable asset table — no external requests) |
| `--output-file string`            | `AUDITOR_OUTPUT_FILE`                | stdout        | `-` is treated as stdout. Required for `xlsx` unless stdout is redirected (it's binary). |
| `--stream`                        | `AUDITOR_STREAM`                     | `false`       | With `-o json`, emit NDJSON (one object per line) instead of an array |
| `--sheet-by string`               | `AUDITOR_SHEET_BY`                   | `provider`    | With `-o xlsx`, split worksheets by one or more `+`-joined dimensions: `none\|provider\|type\|region\|account\|tag:KEY` (e.g. `tag:compartment_id`, or `region+tag:compartment_id` for a sheet per region/compartment labelled `region (compartment)`) |
| `--summary`                       | `AUDITOR_SUMMARY`                    | `false`       | With `-o xlsx`, prepend a Summary worksheet (totals + per-sheet and per-type counts, each per-sheet row linked to its worksheet) |
| `--include-raw`                   | `AUDITOR_INCLUDE_RAW`                | `false`       | Attach the full upstream SDK payload to each `Asset.Raw` |
| `--max-concurrency int`           | `AUDITOR_MAX_CONCURRENCY`            | `5`           | Per-provider parallelism cap |
| `--timeout duration`              | `AUDITOR_TIMEOUT`                    | `10m`         | Overall audit timeout |
| `--config string`                 | n/a (flag-only)                      | (see above)   | Override the config-file search path |
| `--oci-profile string`            | `AUDITOR_OCI_PROFILE`                | (DEFAULT)     | `~/.oci/config` profile name |
| `--oci-regions strings`           | `AUDITOR_OCI_REGIONS`                | (all subscribed regions) | Comma-separated list to narrow; falls back to home region if the subscription lookup fails |
| `--oci-compartments strings`      | `AUDITOR_OCI_COMPARTMENTS`           | (all accessible compartments) | Comma-separated compartment OCIDs or names to narrow to. **Subtree-inclusive**: a selected compartment also pulls in its child compartments. Names are case-insensitive; a name matching several compartments selects all of them. Tenancy-global IAM (users/groups) still collects regardless. A selector matching nothing is reported as a (non-fatal) error |
| `--kube-context string`           | `AUDITOR_KUBE_CONTEXT`               | (current)     | kubeconfig context to use (single cluster) |
| `--kube-contexts strings`         | `AUDITOR_KUBE_CONTEXTS`              | (none)        | Scan several clusters in one audit: comma-separated context names, or the literal `all` for every context in the kubeconfig. Overrides `--kube-context`. Each cluster's assets carry that context's cluster name in `account_id`. |
| `--kube-namespace string`         | `AUDITOR_KUBE_NAMESPACE`             | (all)         | Restrict to a single namespace |
| `--kube-exclude-namespaces strings` | `AUDITOR_KUBE_EXCLUDE_NAMESPACES`  | `kube-system,kube-public,kube-node-lease` | Skip these namespaces |
| `--kube-exclude-helm-secrets`     | `AUDITOR_KUBE_EXCLUDE_HELM_SECRETS` | `false`       | Skip Helm v3 release-state Secrets (type `helm.sh/release.v1`) |
| `--kube-exclude-events`           | `AUDITOR_KUBE_EXCLUDE_EVENTS`        | `false`       | Skip `Event` objects (core `v1` + `events.k8s.io`); they're high-volume and ephemeral, so this is dropped at discovery before any `list` |
| `--netbird-management-url string` | `AUDITOR_NETBIRD_MANAGEMENT_URL` / `NETBIRD_MANAGEMENT_URL` | (NetBird cloud) | NetBird self-hosted Management API base URL. Auth is the Personal Access Token in `NETBIRD_API_TOKEN` |
| `--tailscale-tailnet string`      | `AUDITOR_TAILSCALE_TAILNET` / `TAILSCALE_TAILNET` | `-` (the token's own tailnet) | Tailnet to inventory. Tailscale is enabled by `TAILSCALE_API_KEY` (an API access token or OAuth client secret) |
| `--tailscale-api-url string`      | `AUDITOR_TAILSCALE_API_URL` / `TAILSCALE_API_BASE_URL` | (Tailscale cloud) | Control-plane API base URL, for self-hosted / Headscale-compatible planes |
| `--gcp-project string`            | `AUDITOR_GCP_PROJECT`               | `$GOOGLE_CLOUD_PROJECT` | GCP project to inventory via Cloud Asset Inventory. GCP is enabled by `GOOGLE_CLOUD_PROJECT` or `GCP_SCOPE`; auth is ADC. Needs `roles/cloudasset.viewer` + the Cloud Asset API enabled |
| `--gcp-scope string`              | `AUDITOR_GCP_SCOPE`                 | `$GCP_SCOPE` / the project | Scope override: `projects/<id>`, `folders/<num>`, or `organizations/<num>` (org/folder = every project underneath). For a folder/org scope with gcloud *user* creds, set `GOOGLE_CLOUD_QUOTA_PROJECT` |
| `--cache`                         | `AUDITOR_CACHE`                     | `false`       | Persist the audit snapshot to the `--db` SQLite cache after collecting |
| `--cache-max-age duration`        | `AUDITOR_CACHE_MAX_AGE`             | `0`           | Reuse a cached snapshot from `--db` instead of running providers when one for the same provider set is newer than this (`0` = always run live; implies `--cache`). Avoids re-pulling every run |

### Exit codes

| Code | Meaning                                                                       |
| ---- | ----------------------------------------------------------------------------- |
| `0`  | Success — every selected provider returned results without error              |
| `1`  | Hard failure (rendering error, unknown flag, invalid output file, …)         |
| `2`  | Partial provider failure — some providers errored but others succeeded; the rendered output is still valid for the providers that completed |

The exit-2 semantics let scripts distinguish "completely broken" from
"some Cloudflare zones came back, OCI timed out" — see init-plan.md §6
invariant 5.

---

## `auditor topology`

Run an audit, infer the request-path graph, and render it.
`--include-raw` is forced on internally (the Kubernetes resolvers parse
Ingress / HTTPRoute payloads); the rendered output omits `raw`.

| Flag                    | Default        | Notes |
| ----------------------- | -------------- | ----- |
| `--provider strings`    | (all)          | Comma-separated subset |
| `--from string`         | (live audit)   | Build the graph from a saved `audit -o json` snapshot (array or NDJSON) instead of running providers — instant; pair the snapshot with `--include-raw` for the K8s payload resolvers |
| `-o`, `--output string` | `json`         | `json`, `dot`, `mermaid`, `d2` (d2lang.com), `graphml` (yEd / Gephi / Cytoscape), `excalidraw`, `html` (standalone interactive force-directed viewer — one self-contained file) |
| `--group-by string`     | (flat)         | Cluster nodes into `provider`, `account`, or `region` subgraphs in the `dot` / `mermaid` / `d2` renderers (ignored by the others) |
| `--detail string`       | `low`          | Diagram detail. `low` = every asset. `medium` = one node per group + resource type. `high` = one node per group — the high-level network diagram. `medium`/`high` bucket by `--group-by` (defaulting to `provider`) |
| `--output-file string`  | stdout         | `-` is treated as stdout |
| `--hostname strings`    | (all)          | Trace only the connected component(s) reachable from these DNS hostnames |
| `--include-orphans`     | `false`        | Keep asset nodes that have no edges |
| `--max-concurrency int` | `5`            | Mirrors `audit --max-concurrency` |
| `--timeout duration`    | `10m`          | Overall audit + resolve timeout |

### High-level vs low-level diagrams

`--detail` decides how much of the graph reaches the page. One graph cannot
serve both audiences: a low-level view of a real inventory is tens of thousands
of nodes — useful for tracing one request path, unreadable as a network
diagram — while a network diagram wants one box per platform.

```bash
# Low level (default): every asset, every edge.
auditor topology -o dot | dot -Tsvg > detail.svg

# Medium: one node per provider + resource type, e.g. "kubernetes · v1.Pod ×128".
auditor topology --detail medium -o mermaid

# High level: one box per provider, arrows weighted by how many underlying
# relationships they stand for.
auditor topology --detail high -o dot | dot -Tsvg > overview.svg

# Group by account or region instead of provider.
auditor topology --detail high --group-by account -o d2
```

At `medium` and `high`:

- Collapsed nodes carry `member_count` / `member_types` tags and a
  `topology.group` type, so downstream tools can tell an aggregate from an asset.
- Collapsed edges carry a `count` (rendered as `×N`), and are marked
  `heuristic` if *any* of the edges they stand for was — a summary arrow is
  only as trustworthy as its weakest constituent.
- Edges that fall **inside** one bucket don't become self-loops; their number
  is reported as the node's `internal_edges` tag instead.

### Traffic-flow edges

Beyond request paths (`dns`, `lb-backend`, `gateway-route`, …), the graph
carries **traffic-flow** edges derived from policy — `traffic-allow` and
`traffic-deny`, coloured green and red in every renderer:

| Source | What it reads |
| ------ | ------------- |
| Kubernetes | `NetworkPolicy` ingress/egress rules (needs raw payloads — forced on by `auditor topology`) |
| Tailscale | The tailnet policy file's `acls`, `grants`, and `ssh` rules |
| NetBird | Policy rules' source/destination groups |

Each rule stays in the graph as a node, so a path reads
`source → rule → destination`. That keeps the edge count linear (a single
"allow everything to everything" rule over 500 peers is 1,000 edges this way
and 250,000 drawn directly) and gives the rule that authorises a flow somewhere
to live, with its ports and action attached.

Wildcard selectors (`*`, `autogroup:*`, an empty NetworkPolicy `from`) are
deliberately left unexpanded: attaching every node to a catch-all rule is the
largest and least informative edge set the graph can produce.

```bash
# Just the policy view — who may reach whom.
auditor topology -o json | jq '.edges[] | select(.kind | startswith("traffic-"))'
```

Provider knobs (`--oci-profile`, `--oci-regions`, `--kube-*`, `--tailscale-*`)
mirror `auditor audit`.

---

## `auditor reach`

Traces reachability over the inferred topology. Edges point the way a request
travels, so `--from` walks forwards and `--to` walks backwards.

| Flag | Default | Notes |
| ---- | ------- | ----- |
| `--from string` | — | Source selector: what can it reach? |
| `--to string` | — | Target selector: what can reach it? Give both to enumerate routes between them |
| `--exposed` | `false` | Start from the estate's public entry points |
| `--max-hops int` | `6` | Maximum path length |
| `--max-paths int` | `25` | Result cap; the report states when it truncated |
| `--kinds strings` | (all but deny) | Restrict traversal to these edge kinds, e.g. `traffic-allow` for a policy-only view |
| `--include-deny` | `false` | Follow `traffic-deny` edges too — see below |
| `-o`, `--output string` | `table` | `table`, `json`, or any topology renderer (`dot`, `mermaid`, `d2`, `graphml`, `excalidraw`, `drawio`, `html`) |
| `--exit-code` | `false` | Exit 1 when any path is found, for CI gating |
| `--from-snapshot string` | (live audit) | Analyse a saved `audit -o json` snapshot instead of running providers |

Selectors are **case-insensitive globs matched against both the asset id and
its name**, so `api.example.com`, `ocid1.loadbalancer.*`, and `*-prod` all work
without saying which field you mean.

```bash
# What is the internet-facing surface?
auditor reach --exposed

# CI gate: fail the build if anything is reachable from outside.
auditor reach --exposed --exit-code

# What can reach the database?
auditor reach --to '*postgres*'

# Only what policy permits — NetworkPolicies, Tailscale ACLs, NetBird rules.
auditor reach --to '*postgres*' --kinds traffic-allow

# Every route from a hostname to a workload, as a diagram.
auditor reach --from api.example.com --to '*pod*' -o dot | dot -Tsvg > path.svg
```

### Two things the output is careful about

**`traffic-deny` edges are not traversed by default.** A deny edge states that
traffic does *not* flow; following it while computing reachability would
manufacture routes that policy forbids. `--include-deny` turns them on for
auditing the denials themselves, and any path that crosses one is flagged.

**Absence of a path is not proof of isolation.** The graph is inferred from
collected relationships, so "no paths found" can equally mean a resolver
couldn't join two providers — for instance a Kubernetes resolver that needs
`--include-raw`, or a provider that wasn't part of the audit. The report says
so explicitly rather than letting an empty result read as a clean bill of
health. Likewise, a result that hits `--max-paths` reports that it truncated:
a capped list read as "these are all the ways in" is the dangerous reading.

---

## `auditor diff`

Compare two audit snapshots (`auditor audit -o json`, array or NDJSON —
sniffed automatically) and report drift. Asset identity is
`(provider, id)`; `Raw` and `CreatedAt` are excluded from comparison.

```bash
auditor diff old.json new.json
auditor diff -o markdown old.json new.json   # paste into a PR
auditor diff --exit-code old.json new.json   # CI gate
```

| Flag                    | Default  | Notes |
| ----------------------- | -------- | ----- |
| `-o`, `--output string` | `table`  | `table`, `json`, `markdown` |
| `--output-file string`  | stdout   | `-` is treated as stdout |
| `--exit-code`           | `false`  | Exit `1` when any drift is found, `0` when clean (mirrors `git diff --exit-code`) |

---

## `auditor serve`

Run the embedded web UI + JSON/SSE API.

| Flag                              | Env / config key            | Default     | Notes |
| --------------------------------- | --------------------------- | ----------- | ----- |
| `--addr string`                   | `AUDITOR_ADDR`              | `:8080`     | Listen address. Use `127.0.0.1:8080` to bind loopback only. |
| `--auth string`                   | `AUDITOR_AUTH`              | `none`      | `none` \| `basic` \| `token` |
| `--provider strings`              | `AUDITOR_PROVIDER`          | (all)       | Scopes API requests that name no providers. An explicit `?providers=` on the request still wins. |
| `--max-concurrency int`           | `AUDITOR_MAX_CONCURRENCY`   | `5`         | Mirrors `audit --max-concurrency`; passed to providers per request |
| `--include-raw`                   | `AUDITOR_INCLUDE_RAW`       | `false`     | Attach SDK payload to each Asset.Raw in SSE + export |

`--provider` exists mainly so `--demo` is safe: on a host that also holds
real credentials, a browser request that omitted the parameter would
otherwise blend the synthetic demo inventory into a real one, and nothing
downstream marks which assets were invented. `serve --demo` therefore
defaults the scope to `demo` alone.

### Server-side env vars

These don't have flags — set them in the operator's environment when
`--auth` requires them:

| Env var                | Used when           | Notes                                                              |
| ---------------------- | ------------------- | ------------------------------------------------------------------ |
| `AUDITOR_BASIC_USER`   | `--auth basic`      | HTTP Basic username                                                |
| `AUDITOR_BASIC_PASS`   | `--auth basic`      | HTTP Basic password (compared in constant time)                   |
| `AUDITOR_API_TOKEN`    | `--auth token`      | Required `Authorization: Bearer <token>` value                    |

`/healthz` always returns 200 unauthenticated; everything under
`/api/v1/*` is gated when `--auth` ≠ `none`.

### Per-request Kubernetes context selection

Provider credentials (Cloudflare token, OCI profile, kubeconfig) come from
the operator's environment at server startup and are never client-controllable.
The one exception is *which* cluster the Kubernetes provider scans:

- `GET /api/v1/kube/contexts` lists the contexts in the server's kubeconfig
  (`{"contexts": [...], "current": "..."}`; empty when there's no kubeconfig or
  the server runs in-cluster). The web UI uses it to render a "Kube contexts"
  picker on the Assets tab.
- `/api/v1/audit`, `/api/v1/audit/export`, and `/api/v1/topology` accept a
  `kube_contexts=ctx-a,ctx-b` (or `all`) query parameter. Each name is
  validated against that same kubeconfig — unknown names are dropped and
  reported via an `init_error` SSE event / the `X-Auditor-Init-Errors` response
  header. This selects among clusters the operator already configured; it can't
  inject new credentials.

---

## `auditor version`

| Flag        | Default | Notes                                                       |
| ----------- | ------- | ----------------------------------------------------------- |
| `--json`    | `false` | Emit a JSON object instead of the human-readable one-liner |

---

## `auditor providers`

No flags. Prints the sorted list of registered provider names.

---

## Local database (`--db`): cache + secrets

A single SQLite database (pure-Go `modernc.org/sqlite`, so the static CGO-free
binary keeps working) backs two features:

| Flag / env                       | Default                                  | Notes |
| -------------------------------- | ---------------------------------------- | ----- |
| `--db string` / `AUDITOR_DB`     | `<user-config-dir>/auditor/auditor.db`   | Persistent (all subcommands). The file is created on first write with `0600` perms |
| `AUDITOR_SECRETS_PASSPHRASE`     | (none)                                   | Passphrase that encrypts/decrypts the secrets vault (AES-256-GCM, scrypt-derived key). Required for `secrets` ops and to load vaulted creds at startup |

**Audit cache** — `auditor audit --cache` writes each snapshot; `--cache-max-age 1h`
serves the most recent snapshot for the same provider set if it's younger than
the cutoff, skipping the providers entirely (instant, zero API calls). The cache
key is the canonicalized provider set, so `--provider netbird` and a full audit
don't collide.

## `auditor secrets`

Store provider credentials encrypted in the local database so you don't
re-export them every run. Secrets are keyed by the **environment variable** the
provider reads (e.g. `NETBIRD_API_TOKEN`, `CLOUDFLARE_API_TOKEN`); at startup
each stored secret is decrypted into the environment **only if that variable
isn't already set** — an explicit env var always wins.

| Subcommand              | Notes |
| ----------------------- | ----- |
| `secrets set NAME [VALUE]` | Store/replace a secret. Omit `VALUE` to be prompted (no-echo on a TTY) or to read it from stdin (piped) |
| `secrets get NAME`        | Print the decrypted value to stdout |
| `secrets list`            | List stored secret names (never the values) |
| `secrets rm NAME`         | Delete a secret |

All of them honor `--passphrase` (default `$AUDITOR_SECRETS_PASSPHRASE`).

```bash
export AUDITOR_SECRETS_PASSPHRASE='…'
auditor secrets set NETBIRD_API_TOKEN nbp_xxx     # or pipe: pbpaste | auditor secrets set NETBIRD_API_TOKEN
auditor audit --provider netbird                  # token loaded from the vault
```

---

## Config file

YAML. Keys mirror the env-var paths (dot-separated). Example:

```yaml
# auditor.yaml
output:
  format: csv
  stream: false
  include-raw: false

audit:
  max-concurrency: 10
  timeout: 30m

provider: [cloudflare, kubernetes]

oci:
  profile: PROD
  regions: [us-ashburn-1, us-phoenix-1]

kube:
  context: prod-cluster
  exclude-namespaces: [kube-system, kube-public, kube-node-lease, istio-system]

# Server-mode keys (used by `auditor serve`):
addr: ":9090"
auth: token
```

The mapping rule: take the env var, drop the `AUDITOR_` prefix,
lowercase, replace `_` with `.` between major sections, keep `-` inside
keys. (Viper handles this normalization with the env-key replacer
configured in `internal/config/config.go`.)

---

## Discovering the current effective config

```bash
auditor audit --provider none -o json     # exit-criterion smoke test
auditor version --json                    # confirms which build is running
auditor providers                          # which providers were registered at init
```

There's no `auditor config --dump` yet — file an issue if you'd find one
useful.
