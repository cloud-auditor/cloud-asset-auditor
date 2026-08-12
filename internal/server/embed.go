package server

import "embed"

// WebFS holds the embedded frontend: the static export of the Next.js app in
// `web/` at the repo root, copied here by `just web`.
//
// The generated tree is committed, not built on demand, so `go build` — and
// therefore CI, goreleaser, and the Docker image — need no Node toolchain.
// Regenerate it with `just web` after changing anything under web/, and
// commit the result alongside the source change; the two drifting apart is
// the one failure mode of this arrangement.
//
// `all:` is required: without it embed skips files whose names begin with
// `_`, and Next.js puts every hashed JS/CSS chunk under `_next/`, so the app
// would ship as an HTML shell with no code.
//
// Deviation from init-plan.md §1 (which puts web/ at the repo root): the
// embed directive requires its referenced files to be in or below the
// declaring file's directory, and keeping them adjacent to the server
// package matches conventional Go layout for embedded assets.
//
//go:embed all:webui
var WebFS embed.FS

// OpenAPISpec is the served OpenAPI 3.1 description of /api/v1/*. The
// handler exposes it verbatim at GET /api/v1/openapi.yaml so client
// generators (oapi-codegen, openapi-typescript, etc.) can consume the
// running server's contract without out-of-band downloads.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
