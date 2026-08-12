package netbird

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// postureCheck is the subset of GET /api/posture-checks we map. The `checks`
// container holds optional, heterogeneous sub-checks; we record which ones are
// present (presence via *json.RawMessage) and keep the full detail in Raw.
type postureCheck struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Checks      struct {
		NBVersion        *json.RawMessage `json:"nb_version_check"`
		OSVersion        *json.RawMessage `json:"os_version_check"`
		GeoLocation      *json.RawMessage `json:"geo_location_check"`
		PeerNetworkRange *json.RawMessage `json:"peer_network_range_check"`
		Process          *json.RawMessage `json:"process_check"`
	} `json:"checks"`
}

func (p *Provider) collectPostureChecks(ctx context.Context, out chan<- core.Asset) error {
	var checks []postureCheck
	if err := p.client.getJSON(ctx, "/api/posture-checks", &checks); err != nil {
		return err
	}
	for _, c := range checks {
		if !sendAsset(ctx, out, p.postureCheckToAsset(c)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) postureCheckToAsset(c postureCheck) core.Asset {
	var present []string
	for name, raw := range map[string]*json.RawMessage{
		"nb_version":         c.Checks.NBVersion,
		"os_version":         c.Checks.OSVersion,
		"geo_location":       c.Checks.GeoLocation,
		"peer_network_range": c.Checks.PeerNetworkRange,
		"process":            c.Checks.Process,
	} {
		if raw != nil {
			present = append(present, name)
		}
	}
	sort.Strings(present) // deterministic tag value regardless of map order

	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.posture_check",
		ID:        c.ID,
		Name:      c.Name,
		Tags: tagsOf(
			"description", c.Description,
			"checks", strings.Join(present, ","),
		),
		Raw: p.rawOf(c),
	}
}
