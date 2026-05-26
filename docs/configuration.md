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

## `auditor audit`

Collect assets from one or more providers and render them as JSON or CSV.

| Flag                              | Env / config key                     | Default       | Notes |
| --------------------------------- | ------------------------------------ | ------------- | ----- |
| `--provider strings`              | `AUDITOR_PROVIDER`                   | (all)         | Comma-separated. Use the literal `none` to run zero providers. |
| `-o`, `--output string`           | `AUDITOR_OUTPUT`                     | `json`        | `json` or `csv` |
| `--output-file string`            | `AUDITOR_OUTPUT_FILE`                | stdout        | `-` is treated as stdout |
| `--stream`                        | `AUDITOR_STREAM`                     | `false`       | With `-o json`, emit NDJSON (one object per line) instead of an array |
| `--include-raw`                   | `AUDITOR_INCLUDE_RAW`                | `false`       | Attach the full upstream SDK payload to each `Asset.Raw` |
| `--max-concurrency int`           | `AUDITOR_MAX_CONCURRENCY`            | `5`           | Per-provider parallelism cap |
| `--timeout duration`              | `AUDITOR_TIMEOUT`                    | `10m`         | Overall audit timeout |
| `--config string`                 | n/a (flag-only)                      | (see above)   | Override the config-file search path |
| `--oci-profile string`            | `AUDITOR_OCI_PROFILE`                | (DEFAULT)     | `~/.oci/config` profile name |
| `--oci-regions strings`           | `AUDITOR_OCI_REGIONS`                | (home region) | Or `all` for every subscribed region |
| `--kube-context string`           | `AUDITOR_KUBE_CONTEXT`               | (current)     | kubeconfig context to use |
| `--kube-namespace string`         | `AUDITOR_KUBE_NAMESPACE`             | (all)         | Restrict to a single namespace |
| `--kube-exclude-namespaces strings` | `AUDITOR_KUBE_EXCLUDE_NAMESPACES`  | `kube-system,kube-public,kube-node-lease` | Skip these namespaces |

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
