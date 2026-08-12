package netbird

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// policy is the subset of GET /api/policies we map. Rules carry the
// source/destination group references; we summarize their count and keep the
// detail in Raw.
type policy struct {
	ID                  string       `json:"id"`
	Name                string       `json:"name"`
	Description         string       `json:"description"`
	Enabled             bool         `json:"enabled"`
	SourcePostureChecks []string     `json:"source_posture_checks"`
	Rules               []policyRule `json:"rules"`
}

// policyRule is one rule inside a policy. Sources/Destinations are the group
// references that make a policy a *traffic-flow* statement rather than a
// label: the topology layer expands them to peers to draw who-may-reach-whom
// edges, so they're mapped rather than left in Raw (the graph must work on a
// snapshot collected without --include-raw).
type policyRule struct {
	ID            string     `json:"id"`
	Name          string     `json:"name"`
	Description   string     `json:"description"`
	Enabled       bool       `json:"enabled"`
	Action        string     `json:"action"`
	Protocol      string     `json:"protocol"`
	Bidirectional bool       `json:"bidirectional"`
	Ports         []string   `json:"ports"`
	Sources       []groupRef `json:"sources"`
	Destinations  []groupRef `json:"destinations"`
}

func (p *Provider) collectPolicies(ctx context.Context, out chan<- core.Asset) error {
	var policies []policy
	if err := p.client.getJSON(ctx, "/api/policies", &policies); err != nil {
		return err
	}
	for _, pol := range policies {
		if !sendAsset(ctx, out, p.policyToAsset(pol)) {
			return nil
		}
		// Rules are emitted individually as well as summarised on the parent:
		// each rule is one traffic-flow statement, and the topology layer needs
		// one addressable node per statement to hang its edges on.
		for _, r := range pol.Rules {
			if !sendAsset(ctx, out, p.policyRuleToAsset(pol, r)) {
				return nil
			}
		}
	}
	return nil
}

func (p *Provider) policyToAsset(pol policy) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.policy",
		ID:        pol.ID,
		Name:      pol.Name,
		Status:    enabledStatus(pol.Enabled),
		Tags: tagsOf(
			"description", pol.Description,
			"enabled", boolStr(pol.Enabled),
			"rules_count", intStr(len(pol.Rules)),
			"source_posture_checks", joinStr(pol.SourcePostureChecks),
		),
		Raw: p.rawOf(pol),
	}
}

// policyRuleToAsset maps one rule. NetBird rule ids are unique only within a
// policy, so the asset id is qualified by the parent policy id.
func (p *Provider) policyRuleToAsset(pol policy, r policyRule) core.Asset {
	name := r.Name
	if name == "" {
		name = pol.Name + " rule"
	}
	action := r.Action
	if action == "" {
		action = "accept"
	}
	// A rule inside a disabled policy carries no traffic regardless of its own
	// flag, so the effective status folds both together — the graph must not
	// draw a flow that a disabled parent has switched off.
	status := action
	if !pol.Enabled || !r.Enabled {
		status = "disabled"
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.accountID,
		Type:      "netbird.policy_rule",
		ID:        pol.ID + "/" + r.ID,
		Name:      name,
		Status:    status,
		Tags: tagsOf(
			"policy_id", pol.ID,
			"policy_name", pol.Name,
			"description", r.Description,
			"action", action,
			"enabled", boolStr(pol.Enabled && r.Enabled),
			"protocol", r.Protocol,
			"ports", joinStr(r.Ports),
			"bidirectional", boolStr(r.Bidirectional),
			"sources", groupIDs(r.Sources),
			"destinations", groupIDs(r.Destinations),
			"source_names", groupNames(r.Sources),
			"destination_names", groupNames(r.Destinations),
		),
		Raw: p.rawOf(r),
	}
}
