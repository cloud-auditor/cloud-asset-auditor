# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Phases 1–7 are shipped.** Foundation, JSON/CSV renderers, full CLI, Cloudflare provider (zones + DNS; 11 stubs), OCI provider (compartment recursion + region resolution + Compute + Load Balancers; 15 stubs), Kubernetes provider (universal via dynamic client + discovery), web UI (`auditor serve`), Docker image (multi-stage → distroless static, non-root), and the Helm chart (`deploy/helm/cloud-asset-auditor/` — supports CronJob and Deployment modes, BYO credentials Secret, read-only ClusterRole). GitHub Actions (Phase 8) is next.

**Before doing anything substantive, read `init-plan.md` end-to-end.** It is the single source of truth for the layout, abstractions, and phase ordering. Do not invent architecture that contradicts it.

## Build / test / lint

The project uses **`just`** (not `make`) as the task runner. Standard recipes: `just build`, `just test`, `just test-update`, `just lint`, `just tidy`, `just run -- <args>`, `just smoke`. Run `just` with no args to list them. Prefer recipes over raw `go` commands so behavior stays consistent across machines and CI.

**SDK choice deviation from the plan:** Phase 2 uses `github.com/cloudflare/cloudflare-go/v4` (the current production generated SDK), not `v2` as init-plan.md §3 specifies — `v2` was an early-access generated SDK that's been superseded. The `v4` API uses `cloudflare.F(value)` to wrap required params and an `AutoPager` iterator pattern (`iter.Next()` / `iter.Current()` / `iter.Err()`).

**Phase 5 frontend deviation:** init-plan.md specifies Alpine.js. The shipped UI is plain vanilla JS instead — keeps the binary self-contained without vendoring third-party JS, smaller payload, simpler review surface. Same feature set: SSE streaming, sort, filter, provider/type facets, CSV/JSON export, sticky header. Lives in `internal/server/web/` (the plan put `web/` at the repo root; embedded assets are conventionally placed inside the package that uses them in Go).

**Phase 6 image size deviation:** init-plan.md §3 Phase 6 calls for `< 30 MB`. Actual size is ~75 MB. The Cloudflare v4 generated SDK, the OCI v65 SDK (70+ service packages), and k8s client-go + apimachinery push the static binary alone to ~73 MB before distroless adds ~2 MB. Hitting <30 MB would require either ripping out provider SDKs (defeating the point) or a build-tag pruning scheme that doesn't exist upstream. Documented in the Dockerfile comments; revisit if a smaller image becomes a hard requirement.

## Architecture (from `init-plan.md`)

The plan is Go-first (see §0 for the rationale; Python is an explicit fallback with a one-to-one substitution list). The design rests on three contracts that must be implemented in Phase 1 **before** any provider code:

- **`core.Asset`** — canonical, intentionally minimal struct. Provider-specific richness lives in the opt-in `Raw json.RawMessage` field, not as new top-level fields. Resist the urge to extend this struct.
- **`core.Provider`** — `Validate(ctx)` + `Collect(ctx) (<-chan Asset, <-chan error)`. Channels are mandatory, not optional: streaming keeps memory bounded against large K8s clusters (50k+ objects) and lets the UI render rows as they arrive.
- **`output.Renderer`** — consumes the asset channel and writes to an `io.Writer`. JSON (array or NDJSON via `--stream`) and CSV (flattens `Tags` into one column).

Providers register themselves into a `registry` map (via package `init()`) so the CLI can enumerate and select them by name (`--provider oci,cloudflare`). New providers are wired into the binary by adding a blank import to `cmd/auditor/main.go` — that's the only place new providers need to be touched outside their own package.

**Optional Configurable interfaces** on the provider side let the CLI push knob values without changing the base contract. Each is type-asserted in `internal/cli/audit.go::applyProviderOptions` and skipped silently when not implemented:

- `core.ConcurrencyConfigurable` — `SetMaxConcurrency(int)`; receives `--max-concurrency`.
- `core.IncludeRawConfigurable` — `SetIncludeRaw(bool)`; receives `--include-raw`.
- `core.ProfileConfigurable` — `SetProfile(string)`; receives `--oci-profile`.
- `core.RegionsConfigurable` — `SetRegions([]string)`; receives `--oci-regions`.
- `core.KubeConfigurable` — bundled `SetKubeContext` / `SetKubeNamespace` / `SetKubeExcludeNamespaces`; receives `--kube-context`, `--kube-namespace`, `--kube-exclude-namespaces` (they're always applied together so one interface suffices).

When adding a new CLI flag that needs to reach providers, extend `providerOptions` and `applyProviderOptions`, declare a new Configurable interface in `internal/core/provider.go`, and implement it on the provider(s) that care.

### Provider-specific gotchas baked into the plan

- **OCI**: must recurse compartments from the tenancy root — the most common omission, handled in `internal/providers/oci/compartments.go` via the SDK's `CompartmentIdInSubtree=true` flag. Auth chain (implemented in `auth.go`): instance principal (gated by a 250 ms IMDS probe so laptops don't pay the cost) → resource principal (gated by `OCI_RESOURCE_PRINCIPAL_VERSION` env) → config file (~/.oci/config, profile from `--oci-profile`) → env vars (`OCI_*` prefix). Resource fan-out is per (region × compartment × resource type); tenancy-global resources (Policies / Users / Groups / DynamicGroups) run once outside the loop.
- **Kubernetes**: uses the **dynamic client + discovery** (`ServerPreferredResources` → `dynamicClient.Resource(gvr).List`), not typed clients. This is what makes CRDs come along for free. Implementation lives in `internal/providers/kubernetes/`. The filtering logic (`filterResources` in `discover.go`) drops subresources (names containing `/`) and anything whose verb list doesn't include `list`. Per-GVR Forbidden / MethodNotSupported errors are swallowed silently — they mean the SA can't see that resource type, which is a permission gap, not a fault. `ServerPreferredResources` partial errors (`*discovery.ErrGroupDiscoveryFailed`) are treated as warnings so a downed aggregated API doesn't kill the whole audit. Auth: in-cluster (when `KUBERNETES_SERVICE_HOST` is set) or kubeconfig (`KUBECONFIG` env, then `~/.kube/config`), with `--kube-context` to override the current-context.
- **Cloudflare**: token-only auth (`CLOUDFLARE_API_TOKEN`), no legacy email+key path. Fan out resource enumerations under an `errgroup` capped by `--max-concurrency` (default 5).

### Cross-cutting invariants (§6 of the plan — "cheap now, expensive later")

These apply to every phase and every PR:

1. **Stream end-to-end.** Never buffer the full asset list.
2. **Plumb `context.Context` through every SDK call.** Ctrl+C must stop work in <1s.
3. **`log/slog` to stderr only.** stdout is reserved for renderer output when `--output-file` is unset.
4. **Never log secrets.** Use a redaction helper at every error-wrapping site.
5. **Partial failure is normal.** If OCI times out, still emit Cloudflare results with an `errors` section and a distinct non-zero exit code (e.g. 2). Don't abort the whole run on one provider's failure.
6. **Version the web API.** `/api/v1/audit`, not `/api/audit`.

## Phase ordering

The plan is structured so each phase is independently shippable. **Do not jump ahead.** Phase 1 (foundation, no providers) must produce a working `auditor audit --provider none -o json` returning `[]` before any provider is touched. Cloudflare is Phase 2 (simplest auth), OCI is Phase 3, Kubernetes is Phase 4. Web UI is Phase 5. Docker/Helm/CI/docs are Phases 6–9 — after Phase 5 the tool is already useful.

When the user requests work, default to executing one phase at a time per §7 of the plan, not the whole document at once.

## Testing strategy (§5)

- Unit tests mock the SDK clients — no real API calls in unit tests.
- Integration tests live behind `//go:build integration` and run nightly against a sandbox tenancy, not on every PR.
- Renderers use **golden files** — feed a fixed `[]Asset`, assert output byte-for-byte.
- Kubernetes tests use `envtest` (controller-runtime's local apiserver), not kind, for speed.
- Coverage target: ≥70% on `internal/core` and `internal/output`. Provider packages will be lower because most code is SDK glue.
