package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// network is the subset of GET /api/networks we map (the "Networks" feature —
// a named grouping of routers, resources, and policies).
type network struct {
	ID                string   `json:"id"`
	Name              string   `json:"name"`
	Description       string   `json:"description"`
	Routers           []string `json:"routers"`
	RoutingPeersCount int      `json:"routing_peers_count"`
	Resources         []string `json:"resources"`
	Policies          []string `json:"policies"`
}

func (p *Provider) collectNetworks(ctx context.Context, out chan<- core.Asset) error {
	var networks []network
	if err := p.client.getJSON(ctx, "/api/networks", &networks); err != nil {
		return err
	}
	for _, n := range networks {
		if !sendAsset(ctx, out, p.networkToAsset(n)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) networkToAsset(n network) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.network",
		ID:        n.ID,
		Name:      n.Name,
		Tags: tagsOf(
			"description", n.Description,
			"routing_peers_count", intStr(n.RoutingPeersCount),
			"routers_count", intStr(len(n.Routers)),
			"resources_count", intStr(len(n.Resources)),
			"policies", joinStr(n.Policies),
		),
		Raw: p.rawOf(n),
	}
}
