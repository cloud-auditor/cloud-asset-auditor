package server

import "embed"

// WebFS holds the entire embedded frontend. Files live under web/*; the
// server uses fs.Sub to strip that prefix when serving HTTP.
//
// Deviation from init-plan.md §1 (which puts web/ at the repo root): the
// embed directive requires its referenced files to be in or below the
// declaring file's directory, and keeping them adjacent to the server
// package matches conventional Go layout for embedded assets.
//
//go:embed all:web
var WebFS embed.FS

// OpenAPISpec is the served OpenAPI 3.1 description of /api/v1/*. The
// handler exposes it verbatim at GET /api/v1/openapi.yaml so client
// generators (oapi-codegen, openapi-typescript, etc.) can consume the
// running server's contract without out-of-band downloads.
//
//go:embed openapi.yaml
var OpenAPISpec []byte
