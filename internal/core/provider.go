package core

import "context"

// Provider collects assets from a single cloud surface (OCI, Cloudflare,
// Kubernetes, …). Collect streams via channels so large inventories begin
// rendering immediately and memory stays bounded.
type Provider interface {
	Name() string

	// Validate performs a cheap credential/connectivity check.
	Validate(ctx context.Context) error

	// Collect launches the audit and returns two channels: assets and
	// errors. Both channels MUST be closed by the implementation when work
	// is done (or ctx is cancelled). Errors are non-fatal by convention;
	// emit one per recoverable failure and continue.
	Collect(ctx context.Context) (<-chan Asset, <-chan error)
}

// ConcurrencyConfigurable is an optional interface providers may implement
// to receive --max-concurrency before Collect. The CLI type-asserts every
// provider against this and calls SetMaxConcurrency when the assertion holds.
type ConcurrencyConfigurable interface {
	SetMaxConcurrency(n int)
}

// IncludeRawConfigurable is the parallel optional interface for --include-raw.
// Providers that can attach the upstream payload to Asset.Raw implement this;
// the CLI calls SetIncludeRaw before Collect when the assertion holds.
type IncludeRawConfigurable interface {
	SetIncludeRaw(b bool)
}
