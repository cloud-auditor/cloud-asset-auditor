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

// ProfileConfigurable receives the value of --oci-profile (or any other
// provider's "use this named credential profile" flag) before Collect.
// Providers that don't have a profile concept simply omit the method.
type ProfileConfigurable interface {
	SetProfile(p string)
}

// RegionsConfigurable receives the value of --oci-regions before Collect.
// The literal "all" is a sentinel meaning "every subscribed region"; the
// provider is responsible for the expansion.
type RegionsConfigurable interface {
	SetRegions(regions []string)
}

// CompartmentsConfigurable receives the value of --oci-compartments before
// Collect. Each entry is a compartment OCID or name; an empty list means
// "every accessible compartment" (the default). The provider decides the
// match semantics (the OCI provider is subtree-inclusive).
type CompartmentsConfigurable interface {
	SetCompartments(compartments []string)
}

// NetbirdConfigurable receives the value of --netbird-management-url before
// Collect — the self-hosted Management API base URL. An empty value leaves the
// provider's default (the NetBird cloud endpoint) in place. Providers that
// aren't NetBird-shaped simply omit the method.
type NetbirdConfigurable interface {
	SetManagementURL(url string)
}

// TailscaleConfigurable receives the Tailscale-specific flags before Collect
// — the tailnet to inventory (--tailscale-tailnet; empty keeps the token's
// default tailnet) and the control-plane base URL (--tailscale-api-url, for
// self-hosted / Headscale-compatible planes). Both are ignored when empty, so
// the env-derived defaults survive. Providers that aren't Tailscale-shaped
// simply omit the methods.
type TailscaleConfigurable interface {
	SetTailnet(name string)
	SetAPIBaseURL(url string)
}

// GCPConfigurable receives the value of --gcp-scope / --gcp-project before
// Collect — the Cloud Asset Inventory search scope (projects/folders/
// organizations/<id>). An empty value leaves the provider's env-derived
// default (GOOGLE_CLOUD_PROJECT / GCP_SCOPE) in place. Providers that aren't
// GCP-shaped simply omit the method.
type GCPConfigurable interface {
	SetScope(scope string)
}

// KubeConfigurable bundles the Kubernetes-flag setters. They're always
// applied together (context picks the cluster; namespace + excludes filter
// what's listed; the helm-secrets and events toggles drop bookkeeping/ephemeral
// objects), so one interface keeps the type-assertion in the CLI tight.
// Providers that aren't Kubernetes-shaped simply omit the methods.
//
// SetKubeContext (singular) and SetKubeContexts (plural) coexist: the plural
// form drives the multi-cluster scan (one audit fanned across several
// kubeconfig contexts, or the literal "all" sentinel for every context), and
// when set it takes precedence over the singular value.
type KubeConfigurable interface {
	SetKubeContext(name string)
	SetKubeContexts(names []string)
	SetKubeNamespace(ns string)
	SetKubeExcludeNamespaces(ns []string)
	SetKubeExcludeHelmSecrets(bool)
	SetKubeExcludeEvents(bool)
}
