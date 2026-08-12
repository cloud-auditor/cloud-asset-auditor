/**
 * Next.js is used in **static export** mode on purpose.
 *
 * The binary must stay self-contained (no Node runtime, no sidecar, no CDN):
 * `next build` emits a pure HTML/CSS/JS tree into `out/`, which is copied to
 * `internal/server/webui/` and embedded with `go:embed`. Go's http.FileServer
 * then serves it with zero JS-runtime involvement.
 *
 * Consequences that constrain the app code:
 *   - No server components that fetch at request time, no Route Handlers, no
 *     middleware, no ISR. Every page is a client component talking to the Go
 *     API at /api/v1/* from the browser.
 *   - `trailingSlash` must stay true. It makes the export emit
 *     `out/assets/index.html` rather than `out/assets.html`; Go's FileServer
 *     resolves a directory request to index.html but will 404 on the
 *     extensionless `/assets` form. Turning this off silently breaks every
 *     route except `/`.
 *   - Image optimization needs a server, so it is disabled.
 *
 * @type {import('next').NextConfig}
 */
const nextConfig = {
  output: 'export',
  trailingSlash: true,
  images: { unoptimized: true },
  // The export is served from a binary that is often run over a plain-HTTP
  // port-forward; a strict-mode double-render would double every SSE
  // connection in development and make stream debugging confusing.
  reactStrictMode: true,

  // Pin the build ID. Next defaults to a random string per build, which lands
  // in the output path (`_next/static/<buildId>/`) and in every page's inlined
  // payload — so two builds of identical source differ.
  //
  // That breaks the arrangement this project depends on: the export is
  // committed to internal/server/webui/ and CI re-builds and diffs it to catch
  // a stale bundle. With a random ID the diff never matches, so the check
  // fails on every run and stops meaning anything.
  //
  // A constant is safe because cache-busting does not rely on this value: the
  // chunk filenames already carry content hashes, and index.html is served
  // `no-cache` (see spaHandler in internal/server/server.go).
  generateBuildId: () => 'auditor',
};

export default nextConfig;
