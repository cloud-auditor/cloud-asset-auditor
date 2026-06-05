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

Collect assets from one or more providers and render them as JSON, CSV, or XLSX.

| Flag                              | Env / config key                     | Default       | Notes |
| --------------------------------- | ------------------------------------ | ------------- | ----- |
| `--provider strings`              | `AUDITOR_PROVIDER`                   | (all)         | Comma-separated. Use the literal `none` to run zero providers. |
| `-o`, `--output string`           | `AUDITOR_OUTPUT`                     | `json`        | `json`, `csv`, or `xlsx` |
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
| `--kube-context string`           | `AUDITOR_KUBE_CONTEXT`               | (current)     | kubeconfig context to use |
| `--kube-namespace string`         | `AUDITOR_KUBE_NAMESPACE`             | (all)         | Restrict to a single namespace |
| `--kube-exclude-namespaces strings` | `AUDITOR_KUBE_EXCLUDE_NAMESPACES`  | `kube-system,kube-public,kube-node-lease` | Skip these namespaces |
| `--kube-exclude-helm-secrets`     | `AUDITOR_KUBE_EXCLUDE_HELM_SECRETS` | `false`       | Skip Helm v3 release-state Secrets (type `helm.sh/release.v1`) |

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

## `auditor serve`

Run the embedded web UI + JSON/SSE API.

| Flag                              | Env / config key            | Default     | Notes |
| --------------------------------- | --------------------------- | ----------- | ----- |
| `--addr string`                   | `AUDITOR_ADDR`              | `:8080`     | Listen address. Use `127.0.0.1:8080` to bind loopback only. |
| `--auth string`                   | `AUDITOR_AUTH`              | `none`      | `none` \| `basic` \| `token` |
| `--max-concurrency int`           | `AUDITOR_MAX_CONCURRENCY`   | `5`         | Mirrors `audit --max-concurrency`; passed to providers per request |
| `--include-raw`                   | `AUDITOR_INCLUDE_RAW`       | `false`     | Attach SDK payload to each Asset.Raw in SSE + export |

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

---

## `auditor version`

| Flag        | Default | Notes                                                       |
| ----------- | ------- | ----------------------------------------------------------- |
| `--json`    | `false` | Emit a JSON object instead of the human-readable one-liner |

---

## `auditor providers`

No flags. Prints the sorted list of registered provider names.

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
