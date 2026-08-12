package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// groupRef is the minimal {id, name} group object embedded in peers, policy
// rules, etc. (the API's GroupMinimum).
type groupRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// group is the subset of GET /api/groups we map.
type group struct {
	ID             string     `json:"id"`
	Name           string     `json:"name"`
	PeersCount     int        `json:"peers_count"`
	ResourcesCount int        `json:"resources_count"`
	Issued         string     `json:"issued"`
	Peers          []groupRef `json:"peers"`
}

func (p *Provider) collectGroups(ctx context.Context, out chan<- core.Asset) error {
	var groups []group
	if err := p.client.getJSON(ctx, "/api/groups", &groups); err != nil {
		return err
	}
	for _, g := range groups {
		if !sendAsset(ctx, out, p.groupToAsset(g)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) groupToAsset(g group) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.group",
		ID:        g.ID,
		Name:      g.Name,
		Tags: tagsOf(
			"peers_count", intStr(g.PeersCount),
			"resources_count", intStr(g.ResourcesCount),
			"issued", g.Issued,
			"peers", groupNames(g.Peers),
			"peer_ids", groupIDs(g.Peers),
		),
		Raw: p.rawOf(g),
	}
}
