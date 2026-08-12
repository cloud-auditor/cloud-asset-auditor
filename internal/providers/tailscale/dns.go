package tailscale

import (
	"context"
	"errors"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// dnsConfig is the assembled view of the tailnet's DNS settings. Tailscale
// splits these across three endpoints; a tailnet has exactly one of each, so
// they're folded into a single `tailscale.dns` asset rather than three
// near-empty ones.
type dnsConfig struct {
	Nameservers []string `json:"nameservers"`
	SearchPaths []string `json:"searchPaths"`
	MagicDNS    bool     `json:"magicDNS"`
}

func (p *Provider) collectDNS(ctx context.Context, out chan<- core.Asset) error {
	var cfg dnsConfig
	var errs []error

	var ns struct {
		DNS []string `json:"dns"`
	}
	if err := p.client.getJSON(ctx, p.tailnetPath("/dns/nameservers"), &ns); err != nil {
		errs = append(errs, err)
	} else {
		cfg.Nameservers = ns.DNS
	}

	var sp struct {
		SearchPaths []string `json:"searchPaths"`
	}
	if err := p.client.getJSON(ctx, p.tailnetPath("/dns/searchpaths"), &sp); err != nil {
		errs = append(errs, err)
	} else {
		cfg.SearchPaths = sp.SearchPaths
	}

	var prefs struct {
		MagicDNS bool `json:"magicDNS"`
	}
	if err := p.client.getJSON(ctx, p.tailnetPath("/dns/preferences"), &prefs); err != nil {
		errs = append(errs, err)
	} else {
		cfg.MagicDNS = prefs.MagicDNS
	}

	// All three failed — nothing worth emitting, report the failure. A
	// partial read still yields a useful asset, with the gaps reported
	// alongside it (invariant 5).
	if len(errs) == 3 {
		return errors.Join(errs...)
	}
	if !sendAsset(ctx, out, p.dnsToAsset(cfg)) {
		return nil
	}
	return errors.Join(errs...)
}

func (p *Provider) dnsToAsset(cfg dnsConfig) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.dns",
		ID:        "dns:" + p.cfg.Tailnet,
		Name:      "DNS settings",
		Status:    "configured",
		Tags: tagsOf(
			"nameservers", joinStr(cfg.Nameservers),
			"search_paths", joinStr(cfg.SearchPaths),
			"magic_dns", boolStr(cfg.MagicDNS),
			"nameserver_count", intStr(len(cfg.Nameservers)),
		),
		Raw: p.rawOf(cfg),
	}
}
