package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// setupKey is the subset of GET /api/setup-keys we map. The secret `key` field
// is DELIBERATELY not declared here so it can never reach Asset.Raw — the list
// endpoint masks it anyway, but omitting it from the struct is belt-and-braces
// (invariant 4: never log/emit secrets).
type setupKey struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	State      string   `json:"state"`
	Valid      bool     `json:"valid"`
	Revoked    bool     `json:"revoked"`
	Expires    string   `json:"expires"`
	LastUsed   string   `json:"last_used"`
	UpdatedAt  string   `json:"updated_at"`
	UsedTimes  int      `json:"used_times"`
	UsageLimit int      `json:"usage_limit"`
	AutoGroups []string `json:"auto_groups"`
	Ephemeral  bool     `json:"ephemeral"`
}

func (p *Provider) collectSetupKeys(ctx context.Context, out chan<- core.Asset) error {
	var keys []setupKey
	if err := p.client.getJSON(ctx, "/api/setup-keys", &keys); err != nil {
		return err
	}
	for _, k := range keys {
		if !sendAsset(ctx, out, p.setupKeyToAsset(k)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) setupKeyToAsset(k setupKey) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.setup_key",
		ID:        k.ID,
		Name:      k.Name,
		Status:    k.State, // already human: valid | overused | expired | revoked
		Tags: tagsOf(
			"type", k.Type,
			"valid", boolStr(k.Valid),
			"revoked", boolStr(k.Revoked),
			"used_times", intStr(k.UsedTimes),
			"usage_limit", intStr(k.UsageLimit),
			"expires", k.Expires,
			"last_used", k.LastUsed,
			"auto_groups", joinStr(k.AutoGroups),
			"ephemeral", boolStr(k.Ephemeral),
		),
		Raw: p.rawOf(k),
	}
}
