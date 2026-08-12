package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// user is the subset of GET /api/users we map. The secret `password` field
// (only ever present on a create response, never on list) is intentionally not
// declared so it can't reach Asset.Raw.
type user struct {
	ID            string   `json:"id"`
	Email         string   `json:"email"`
	Name          string   `json:"name"`
	Role          string   `json:"role"`
	Status        string   `json:"status"`
	IsBlocked     bool     `json:"is_blocked"`
	IsServiceUser bool     `json:"is_service_user"`
	LastLogin     string   `json:"last_login"`
	AutoGroups    []string `json:"auto_groups"`
	Issued        string   `json:"issued"`
	IdpID         string   `json:"idp_id"`
}

func (p *Provider) collectUsers(ctx context.Context, out chan<- core.Asset) error {
	var users []user
	if err := p.client.getJSON(ctx, "/api/users", &users); err != nil {
		return err
	}
	for _, u := range users {
		if !sendAsset(ctx, out, p.userToAsset(u)) {
			return nil
		}
	}
	return nil
}

func (p *Provider) userToAsset(u user) core.Asset {
	name := u.Name
	if name == "" {
		name = u.Email
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.user",
		ID:        u.ID,
		Name:      name,
		Status:    u.Status, // active | invited | blocked
		Tags: tagsOf(
			"email", u.Email,
			"role", u.Role,
			"is_blocked", boolStr(u.IsBlocked),
			"is_service_user", boolStr(u.IsServiceUser),
			"last_login", u.LastLogin,
			"auto_groups", joinStr(u.AutoGroups),
			"issued", u.Issued,
			"idp_id", u.IdpID,
		),
		Raw: p.rawOf(u),
	}
}
