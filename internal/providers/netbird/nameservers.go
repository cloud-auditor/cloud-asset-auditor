package netbird

import (
	"context"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// nameserverGroup is the subset of GET /api/dns/nameservers we map (a NetBird
// "nameserver group" — a set of upstream resolvers distributed to peer groups
// for a set of match domains).
type nameserverGroup struct {
	ID                   string       `json:"id"`
	Name                 string       `json:"name"`
	Description          string       `json:"description"`
	Nameservers          []nameserver `json:"nameservers"`
	Enabled              bool         `json:"enabled"`
	Groups               []string     `json:"groups"`
	Primary              bool         `json:"primary"`
	Domains              []string     `json:"domains"`
	SearchDomainsEnabled bool         `json:"search_domains_enabled"`
}

type nameserver struct {
	IP     string `json:"ip"`
	Port   int    `json:"port"`
	NSType string `json:"ns_type"`
}

func (p *Provider) collectNameservers(ctx context.Context, out chan<- core.Asset) error {
	var groups []nameserverGroup
	if err := p.client.getJSON(ctx, "/api/dns/nameservers", &groups); err != nil {
		return err
	}
	for _, g := range groups {
		if !sendAsset(ctx, out, p.nameserverGroupToAsset(g)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) nameserverGroupToAsset(g nameserverGroup) core.Asset {
	ips := make([]string, 0, len(g.Nameservers))
	for _, ns := range g.Nameservers {
		if ns.IP != "" {
			ips = append(ips, ns.IP)
		}
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.nameserver",
		ID:        g.ID,
		Name:      g.Name,
		Status:    enabledStatus(g.Enabled),
		Tags: tagsOf(
			"description", g.Description,
			"nameservers", strings.Join(ips, ","),
			"domains", joinStr(g.Domains),
			"groups", joinStr(g.Groups),
			"primary", boolStr(g.Primary),
			"search_domains_enabled", boolStr(g.SearchDomainsEnabled),
		),
		Raw: p.rawOf(g),
	}
}
