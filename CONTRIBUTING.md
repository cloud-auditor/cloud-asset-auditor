# Contributing

Thanks for helping make cloud-asset-auditor better. This file is the
short version — the long version lives in the docs the project already
ships and is linked from each section below.

## Dev setup

Need: **Go 1.26+** and [`just`](https://github.com/casey/just).
Optional: `golangci-lint`, `helm`, `docker`, `kubectl` (for the parts of
CI you want to run locally).

```bash
git clone https://github.com/cloud-auditor/cloud-asset-auditor.git
cd cloud-asset-auditor
just tidy    # pulls deps + generates go.sum
just build   # → ./bin/auditor
just test    # race + cover; should be green before you push
just         # lists every recipe
```

## Where things live

The map is in [CLAUDE.md](./CLAUDE.md#per-phase-file--concern-map);
copying it here would just rot. The high-level picture:

- **`internal/core/`** — `Asset`, `Edge`, `Provider`, registry, optional `Configurable` interfaces. **Don't extend the `Asset` struct.** Provider-specific richness goes into `Asset.Raw` (opt in with `--include-raw`).
- **`internal/providers/<name>/`** — one package per cloud. Each registers itself via `init() { core.Register(name, factory) }` and gets blank-imported in `cmd/auditor/main.go`.
- **`internal/output/`** — JSON / CSV renderers consuming the asset channel. **Streaming is mandatory** — no full buffering.
- **`internal/server/`** — embedded SPA + `/api/v1/*` endpoints.
- **`internal/topology/`** — derived graph + four renderers (JSON / DOT / Mermaid / Excalidraw).
- **`deploy/`** — Dockerfile (Phase 6) and Helm chart (Phase 7).
- **`.github/`** — workflows + reusable composite action.
- **`docs/`** — user-facing reference.

## Required reading before substantive changes

1. **[init-plan.md](./init-plan.md)** — the original phased spec. Don't deviate silently; note deviations explicitly in the commit message and `CLAUDE.md`.
2. **[CLAUDE.md](./CLAUDE.md)** — architecture overview + the cross-cutting invariants from `init-plan.md §6` (stream end-to-end, plumb `context.Context`, `log/slog` to stderr, no secrets in logs, partial failure ≠ fatal, versioned API surface).
3. **[docs/extending.md](./docs/extending.md)** — the worked example for adding a new provider in ~200 lines.

## Common contributions

### Filling in a stubbed resource

Look in:

- `internal/providers/cloudflare/stubs.go` — 11 resource types still stubbed (R2, KV, Workers, D1, Pages, Access, Tunnels, Load Balancers, Rulesets, Page Rules, Certificates).
- `internal/providers/oci/stubs.go` — 15 resource types still stubbed (Block / Boot volumes, VCNs, Subnets, Object Storage, Autonomous DBs, DB Systems, Functions, Container Instances, OKE, Vaults, Policies, Users, Groups, Dynamic Groups).

The orchestrator already calls each stub; the work is the per-resource list/map function. Use `internal/providers/cloudflare/dns.go` or `internal/providers/oci/compute.go` as templates.

### Adding a whole new provider

End-to-end walk-through in [docs/extending.md](./docs/extending.md). Short version: provider package + `init()` registration + blank import in `cmd/auditor/main.go`. The CLI, web UI, renderers, Docker image, Helm chart, and CI all pick it up automatically.

### Adding a new CLI flag

Touch points:

- `internal/cli/audit.go` — declare the flag, add a field to `providerOptions`.
- If providers should receive it: declare a `*Configurable` interface in `internal/core/provider.go`, add the corresponding setter call in `applyProviderOptions`, and have each provider opt in by implementing the method.
- `docs/configuration.md` — the flag table.

## Conventions

The codebase tries to keep these invariants in every commit:

- **`Asset` struct stays minimal.** Provider-specific fields go in `Asset.Raw` (opt-in via `--include-raw`). Don't add new top-level fields.
- **Channels in `Provider.Collect`.** Both channels must close exactly once. Errors are non-fatal: push to `errs` and keep going.
- **Partial failure is not a crash.** Per-resource failures are warnings; per-provider initialization failures are warnings. The CLI maps "some assets, some errors" to exit code 2.
- **`context.Context` plumbed through every SDK call.** `Ctrl+C` should stop in <1 s.
- **`log/slog` to stderr only.** stdout is reserved for renderer output when `--output-file` is unset.
- **No secrets in logs.** Ever.
- **Comments explain WHY, not WHAT.** Well-named identifiers cover WHAT. Match the style of nearby code.
- **Versioned API.** Anything under `/api/` should be under `/api/v1/`.

## Tests

```bash
just test           # everything, with -race -cover
just test-update    # regenerate renderer golden files
```

What to write:

- **Pure function tests** — mapping helpers, format renderers, filter logic. High signal, easy to maintain.
- **Provider mapping tests** — feed a synthetic SDK struct into `xxxToAsset`, assert the resulting `Asset`. Don't mock the SDK client itself for the baseline ship.
- **Integration tests** — anything that hits a real cloud goes behind `//go:build integration` and runs in a nightly workflow (not on every PR). Pattern's outlined in `init-plan.md §5`.

## Linting

CI runs `golangci-lint v2.x.x` (installed from source so it matches the project's Go toolchain). To reproduce locally:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
golangci-lint run ./...
```

Config in [`.golangci.yml`](./.golangci.yml). If you disagree with a rule, prefer fixing the rule or scoping an `exclusions.rules` entry over sprinkling `//nolint`.

## Commit messages

Conventional-commits friendly but not strict. A reasonable shape:

```
<scope>: short imperative summary in under 70 chars

Longer explanation: what changed, why, and any non-obvious trade-offs.
Wrap at 72 chars. Reference issues by URL, not just by number.

Co-Authored-By: ... (optional)
```

The release workflow's changelog generator groups by the conventional
prefixes (`feat:` / `fix:`) — see [`.goreleaser.yaml`](./.goreleaser.yaml).

## Pull-request flow

1. Branch off `main`.
2. Keep commits small and focused. One logical change per commit.
3. Run `just test` (and ideally `just lint`) before pushing.
4. Open a PR. CI runs five gates (test, lint, gosec, helm-lint, smoke) on every push to the branch.
5. Squash isn't required, but rebasing onto `main` to keep history linear is appreciated.
6. Don't force-push to `main`. Don't push directly to `main` unless the change is genuinely trivial (typo in a doc, dependency bump caught by Dependabot).

## Releases

Push a tag matching `v*` (e.g. `v0.2.0`):

- [`release.yml`](./.github/workflows/release.yml) runs `goreleaser` → cross-platform archives + SHA256 checksums + cosign-keyless signature + SBOM + GitHub Release.
- [`docker.yml`](./.github/workflows/docker.yml) builds + pushes the multi-arch image to `ghcr.io/cloud-auditor/cloud-asset-auditor:<semver>`, signed.

Bump the Helm chart's `version` (chart packaging) and `appVersion` (image tag) in [`deploy/helm/cloud-asset-auditor/Chart.yaml`](./deploy/helm/cloud-asset-auditor/Chart.yaml) in the same commit.

## Reporting issues

GitHub Issues. Include:

- `auditor version` output.
- The exact command + flags / config that reproduced the problem.
- Provider + region (for OCI / Cloudflare); cluster version (for Kubernetes).
- Whether the failure is reproducible or intermittent.

For security issues, do NOT use a public issue — until a `SECURITY.md` lands with a private contact, email the maintainers listed in [`Chart.yaml`](./deploy/helm/cloud-asset-auditor/Chart.yaml).

## License

No `LICENSE` file is committed yet; contributions are accepted on the understanding that they'll be relicensed under whatever the maintainers eventually choose (almost certainly OSI-approved permissive). If that worries you, open an issue and we'll prioritize picking one.
