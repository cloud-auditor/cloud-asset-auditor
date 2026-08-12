package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// route is the subset of GET /api/routes we map. `network` (a CIDR) and
// `domains` are the network identifiers; peer / peer_groups / groups are id
// references to the routing and consumer peers.
type route struct {
	ID          string   `json:"id"`
	NetworkID   string   `json:"network_id"`
	Enabled     bool     `json:"enabled"`
	Network     string   `json:"network"`
	NetworkType string   `json:"network_type"`
	Domains     []string `json:"domains"`
	Peer        string   `json:"peer"`
	PeerGroups  []string `json:"peer_groups"`
	Groups      []string `json:"groups"`
	Masquerade  bool     `json:"masquerade"`
	Metric      int      `json:"metric"`
	Description string   `json:"description"`
}

func (p *Provider) collectRoutes(ctx context.Context, out chan<- core.Asset) error {
	var routes []route
	if err := p.client.getJSON(ctx, "/api/routes", &routes); err != nil {
		return err
	}
	for _, r := range routes {
		if !sendAsset(ctx, out, p.routeToAsset(r)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) routeToAsset(r route) core.Asset {
	name := r.NetworkID
	if name == "" {
		name = r.Network
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.route",
		ID:        r.ID,
		Name:      name,
		Status:    enabledStatus(r.Enabled),
		Tags: tagsOf(
			"network", r.Network,
			"network_type", r.NetworkType,
			"domains", joinStr(r.Domains),
			"peer", r.Peer,
			"peer_groups", joinStr(r.PeerGroups),
			"groups", joinStr(r.Groups),
			"masquerade", boolStr(r.Masquerade),
			"metric", intStr(r.Metric),
			"description", r.Description,
		),
		Raw: p.rawOf(r),
	}
}
