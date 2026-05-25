# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

**Phases 1–2 (partial) are shipped.** The foundation, JSON/CSV renderers, full CLI, and the Cloudflare provider (zones + DNS implemented; 11 other resources stubbed in `internal/providers/cloudflare/stubs.go`) are in place. OCI (Phase 3), Kubernetes (Phase 4), and the web UI (Phase 5) are not started.

**Before doing anything substantive, read `init-plan.md` end-to-end.** It is the single source of truth for the layout, abstractions, and phase ordering. Do not invent architecture that contradicts it.

## Build / test / lint

The project uses **`just`** (not `make`) as the task runner. Standard recipes: `just build`, `just test`, `just test-update`, `just lint`, `just tidy`, `just run -- <args>`, `just smoke`. Run `just` with no args to list them. Prefer recipes over raw `go` commands so behavior stays consistent across machines and CI.

**SDK choice deviation from the plan:** Phase 2 uses `github.com/cloudflare/cloudflare-go/v4` (the current production generated SDK), not `v2` as init-plan.md §3 specifies — `v2` was an early-access generated SDK that's been superseded. The `v4` API uses `cloudflare.F(value)` to wrap required params and an `AutoPager` iterator pattern (`iter.Next()` / `iter.Current()` / `iter.Err()`).

## Architecture (from `init-plan.md`)

The plan is Go-first (see §0 for the rationale; Python is an explicit fallback with a one-to-one substitution list). The design rests on three contracts that must be implemented in Phase 1 **before** any provider code:

- **`core.Asset`** — canonical, intentionally minimal struct. Provider-specific richness lives in the opt-in `Raw json.RawMessage` field, not as new top-level fields. Resist the urge to extend this struct.
- **`core.Provider`** — `Validate(ctx)` + `Collect(ctx) (<-chan Asset, <-chan error)`. Channels are mandatory, not optional: streaming keeps memory bounded against large K8s clusters (50k+ objects) and lets the UI render rows as they arrive.
- **`output.Renderer`** — consumes the asset channel and writes to an `io.Writer`. JSON (array or NDJSON via `--stream`) and CSV (flattens `Tags` into one column).

Providers register themselves into a `registry` map (via package `init()`) so the CLI can enumerate and select them by name (`--provider oci,cloudflare`). New providers are wired into the binary by adding a blank import to `cmd/auditor/main.go` — that's the only place new providers need to be touched outside their own package.

Two **optional** interfaces on the provider side let the CLI push knob values without changing the base contract:

- `core.ConcurrencyConfigurable` — `SetMaxConcurrency(int)`; receives `--max-concurrency` before `Collect`.
- `core.IncludeRawConfigurable` — `SetIncludeRaw(bool)`; receives `--include-raw`.

Providers that don't care simply omit the methods. `internal/cli/audit.go::applyProviderOptions` type-asserts both and is a no-op when the assertion fails.

### Provider-specific gotchas baked into the plan

- **OCI**: must recurse compartments from the tenancy root — the most common omission. Auth chain is instance principal → resource principal → config file → env, with `--oci-profile` to override. One goroutine per region from `--oci-regions`.
- **Kubernetes**: use the **dynamic client + discovery** (`ServerPreferredResources` → `dynamicClient.Resource(gvr).List`), not typed clients. This is what makes CRDs work without code changes. Skip subresources; warn-don't-fail on resources the SA can't list.
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
