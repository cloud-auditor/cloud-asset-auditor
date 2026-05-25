# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Repository state

This is a **greenfield project**. The only artifact present is `init-plan.md`, a phased implementation spec for building "Cloud Asset Auditor" — a CLI + web UI that inventories assets across OCI, Cloudflare, and Kubernetes. There is no `go.mod`, no source tree, no `Makefile`, no tests.

**Before doing anything substantive, read `init-plan.md` end-to-end.** It is the single source of truth for the language choice, layout, abstractions, and phase ordering. Do not invent architecture that contradicts it.

## Build / test / lint

None of these exist yet. They will be added in Phase 1 (`Makefile` targets: `build`, `test`, `lint`, `run`, `docker`). Once present, prefer the `Makefile` targets over raw `go` commands so behavior stays consistent with CI.

## Architecture (from `init-plan.md`)

The plan is Go-first (see §0 for the rationale; Python is an explicit fallback with a one-to-one substitution list). The design rests on three contracts that must be implemented in Phase 1 **before** any provider code:

- **`core.Asset`** — canonical, intentionally minimal struct. Provider-specific richness lives in the opt-in `Raw json.RawMessage` field, not as new top-level fields. Resist the urge to extend this struct.
- **`core.Provider`** — `Validate(ctx)` + `Collect(ctx) (<-chan Asset, <-chan error)`. Channels are mandatory, not optional: streaming keeps memory bounded against large K8s clusters (50k+ objects) and lets the UI render rows as they arrive.
- **`output.Renderer`** — consumes the asset channel and writes to an `io.Writer`. JSON (array or NDJSON via `--stream`) and CSV (flattens `Tags` into one column).

Providers register themselves into a `registry` map so the CLI can enumerate and select them by name (`--provider oci,cloudflare`).

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
