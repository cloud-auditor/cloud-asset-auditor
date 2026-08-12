# Web UI

The auditor's frontend: a Next.js (App Router) app, **statically exported** and
embedded into the Go binary.

## How it fits together

```
web/                        ← source (this directory)
  └─ npm run build          ← Next.js static export
       └─ web/out/          ← plain HTML/CSS/JS, no Node runtime needed
            └─ copied to internal/server/webui/   ← committed
                 └─ //go:embed all:webui          ← served by the Go binary at /
```

One command does the middle three steps:

```bash
just web        # rebuild the export and refresh internal/server/webui/
```

**The generated tree is committed on purpose.** `go build`, CI, goreleaser, and
the Docker image must work without a Node toolchain, so the build output is a
checked-in artefact rather than a build step. The cost is that `web/` and
`internal/server/webui/` can drift: after changing anything here, run `just web`
and commit the regenerated tree alongside your source change. `just web-verify`
(run in CI) rebuilds and fails if the two disagree.

## Developing

The dev server has no proxy, so run the Go API alongside it:

```bash
just build && ./bin/auditor serve   # terminal 1 — API on :8080
just web-dev                        # terminal 2 — UI on :3000
```

Requests to `/api/v1/*` are same-origin in production but cross-origin in dev.
If the browser blocks them, run the dev UI through the Go server instead — build
with `just web`, then just use `:8080`. For most changes (styling, layout,
component logic) the hot-reload loop is worth the occasional rebuild.

```bash
just web-check   # tsc --noEmit
```

## Constraints

These are not stylistic preferences — the export mode enforces them:

- **No server components that fetch, no Route Handlers, no middleware, no ISR.**
  Every page is a client component calling the Go API from the browser.
- **`trailingSlash: true` must stay on.** It makes the export emit
  `out/assets/index.html` rather than `out/assets.html`. Go's `http.FileServer`
  resolves a directory to its `index.html` but 404s the extensionless form, so
  turning this off silently breaks every route except `/`.
- **`//go:embed all:webui`, not `embed webui`.** Without `all:`, embed skips
  underscore-prefixed names — and every hashed chunk lives under `_next/`, so
  the app would ship as an HTML shell with no JavaScript.
- **Dependencies are pinned exactly**, not caret-ranged, because the build
  output is committed and must be reproducible.
- **Install with `--omit=optional`.** That skips `sharp`, Next's image
  optimizer: this is a static export with `images.unoptimized`, so sharp is
  never invoked, but it ships a native libvips with open CVEs.

`internal/server/webui_test.go` asserts most of the above against the embedded
FS, so a violation fails `go test` rather than showing up as a blank page.

## Layout

| Path | What it is |
| ---- | ---------- |
| `app/page.tsx` | Dashboard — inventory shape, per-provider/type/region breakdowns |
| `app/assets/page.tsx` | Streamed asset table with facets, search, and exports |
| `app/topology/page.tsx` | Network diagram: detail levels, grouping, edge-kind filters |
| `components/AuditProvider.tsx` | App-wide audit state; holds the SSE result across navigation |
| `components/GraphCanvas.tsx` | The SVG graph: force layout, pan/zoom/drag, inspector |
| `lib/api.ts` | Typed client for `/api/v1/*`, including the SSE parser |
| `lib/layout.ts` | Deterministic force-directed layout |
| `lib/colors.ts` | Provider and edge-kind palettes (mirrors `topology/render.go`) |

## Why the graph is hand-rolled

`GraphCanvas` + `lib/layout.ts` implement a small Fruchterman–Reingold
simulation rather than pulling in d3-force / Cytoscape / React Flow. The graph
view has a readable ceiling of a few hundred nodes — past that the answer is
`detail=medium|high` (which collapses thousands of assets into tens of nodes)
or a GraphML export into yEd/Gephi, not a faster layout engine. A graph library
would add hundreds of KB to a binary that is already ~75 MB to buy capability
the product deliberately doesn't use.

The layout is seeded from a hash of each node's key, not `Math.random()`, so the
same inventory always draws the same picture — for two people looking at the
same graph, and across React re-renders.
