package tailscale

import (
	"context"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// authKey is the subset of GET /tailnet/{tailnet}/keys we map.
//
// The `key` field — the secret material, only ever populated at creation
// time — has no struct field on purpose, so it cannot reach Asset.Raw even
// with --include-raw. The list endpoint never returns it anyway; omitting the
// field makes that guarantee structural rather than incidental.
type authKey struct {
	ID           string `json:"id"`
	KeyType      string `json:"keyType"`
	Description  string `json:"description"`
	UserID       string `json:"userId"`
	Created      string `json:"created"`
	Expires      string `json:"expires"`
	Revoked      string `json:"revoked"`
	Invalid      bool   `json:"invalid"`
	Capabilities struct {
		Devices struct {
			Create struct {
				Reusable      bool     `json:"reusable"`
				Ephemeral     bool     `json:"ephemeral"`
				Preauthorized bool     `json:"preauthorized"`
				Tags          []string `json:"tags"`
			} `json:"create"`
		} `json:"devices"`
	} `json:"capabilities"`
	Scopes []string `json:"scopes"`
	Tags   []string `json:"tags"`
}

type keyList struct {
	Keys []authKey `json:"keys"`
}

// collectKeys lists auth keys and trust credentials. `all=true` widens the
// response past machine auth keys to OAuth clients and federated identities —
// they're credentials into the tailnet too, so an inventory that omits them
// under-reports the access surface.
func (p *Provider) collectKeys(ctx context.Context, out chan<- core.Asset) error {
	var list keyList
	if err := p.client.getJSON(ctx, p.tailnetPath("/keys?all=true"), &list); err != nil {
		return err
	}
	now := time.Now()
	for _, k := range list.Keys {
		if !sendAsset(ctx, out, p.keyToAsset(k, now)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) keyToAsset(k authKey, now time.Time) core.Asset {
	name := k.Description
	if name == "" {
		name = k.ID
	}
	create := k.Capabilities.Devices.Create
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.key",
		ID:        k.ID,
		Name:      name,
		Status:    keyStatus(k, now),
		CreatedAt: parseTime(k.Created),
		Tags: tagsOf(
			"key_type", k.KeyType,
			"description", k.Description,
			"user_id", k.UserID,
			"expires", k.Expires,
			"revoked", k.Revoked,
			"reusable", boolStr(create.Reusable),
			"ephemeral", boolStr(create.Ephemeral),
			"preauthorized", boolStr(create.Preauthorized),
			"device_tags", joinStr(create.Tags),
			"scopes", joinStr(k.Scopes),
			"acl_tags", joinStr(k.Tags),
		),
		Raw: p.rawOf(k),
	}
}
