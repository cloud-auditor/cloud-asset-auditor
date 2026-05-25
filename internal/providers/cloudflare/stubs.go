package cloudflare

// Stub collectors — registered in the orchestrator so the wiring is in
// place but emit no assets until implemented. Each maps directly to a
// resource type listed in init-plan.md §3 Phase 2.
//
// Implementation order (planned):
//   1. R2.Buckets        — account-scoped, simplest list op
//   2. KV.Namespaces     — account-scoped
//   3. Workers.Scripts   — account-scoped, then per-zone routes
//   4. Pages.Projects    — account-scoped
//   5. Load Balancers    — per-zone
//   6. Rulesets          — account + per-zone
//   7. Page Rules        — per-zone
//   8. Access            — account-scoped
//   9. Tunnels           — account-scoped (Zero Trust)
//  10. D1.Databases      — account-scoped
//  11. Certificates      — account-scoped (custom + edge)
//
// Adding one is mechanical: list → loop → map → send via sendAsset, the
// same shape as dns.go.

import (
	"context"

	"github.com/cloudflare/cloudflare-go/v4/zones"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Account-scoped stubs.
func (p *Provider) collectR2(_ context.Context, _ chan<- core.Asset) error              { return nil }
func (p *Provider) collectKV(_ context.Context, _ chan<- core.Asset) error              { return nil }
func (p *Provider) collectWorkers(_ context.Context, _ chan<- core.Asset) error         { return nil }
func (p *Provider) collectD1(_ context.Context, _ chan<- core.Asset) error              { return nil }
func (p *Provider) collectPages(_ context.Context, _ chan<- core.Asset) error           { return nil }
func (p *Provider) collectAccessApps(_ context.Context, _ chan<- core.Asset) error      { return nil }
func (p *Provider) collectTunnels(_ context.Context, _ chan<- core.Asset) error         { return nil }
func (p *Provider) collectCertificates(_ context.Context, _ chan<- core.Asset) error    { return nil }
func (p *Provider) collectAccountRulesets(_ context.Context, _ chan<- core.Asset) error { return nil }

// Per-zone stubs.
func (p *Provider) collectPageRules(_ context.Context, _ zones.Zone, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectLoadBalancers(_ context.Context, _ zones.Zone, _ chan<- core.Asset) error {
	return nil
}
func (p *Provider) collectZoneRulesets(_ context.Context, _ zones.Zone, _ chan<- core.Asset) error {
	return nil
}
