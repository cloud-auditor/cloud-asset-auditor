package tailscale

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// user is the subset of GET /tailnet/{tailnet}/users we map.
type user struct {
	ID                 string `json:"id"`
	DisplayName        string `json:"displayName"`
	LoginName          string `json:"loginName"`
	TailnetID          string `json:"tailnetId"`
	Created            string `json:"created"`
	Type               string `json:"type"`
	Role               string `json:"role"`
	Status             string `json:"status"`
	DeviceCount        int    `json:"deviceCount"`
	LastSeen           string `json:"lastSeen"`
	CurrentlyConnected bool   `json:"currentlyConnected"`
}

type userList struct {
	Users []user `json:"users"`
}

func (p *Provider) collectUsers(ctx context.Context, out chan<- core.Asset) error {
	var list userList
	if err := p.client.getJSON(ctx, p.tailnetPath("/users"), &list); err != nil {
		return err
	}
	for _, u := range list.Users {
		if !sendAsset(ctx, out, p.userToAsset(u)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) userToAsset(u user) core.Asset {
	name := u.DisplayName
	if name == "" {
		name = u.LoginName
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.user",
		ID:        u.ID,
		Name:      name,
		Status:    u.Status,
		CreatedAt: parseTime(u.Created),
		Tags: tagsOf(
			"login_name", u.LoginName,
			"display_name", u.DisplayName,
			"role", u.Role,
			"user_type", u.Type,
			"tailnet_id", u.TailnetID,
			"device_count", intStr(u.DeviceCount),
			"currently_connected", boolStr(u.CurrentlyConnected),
			"last_seen", u.LastSeen,
		),
		Raw: p.rawOf(u),
	}
}
