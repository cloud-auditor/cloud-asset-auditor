package tailscale

import (
	"context"
	"fmt"
	"sort"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// aclPolicy is the subset of the tailnet policy file we map. The client asks
// for `Accept: application/json`, so the API hands back plain JSON rather than
// the HuJSON (comments + trailing commas) the admin console shows — no
// tolerant parser needed.
type aclPolicy struct {
	ACLs      []aclRule           `json:"acls"`
	Grants    []aclRule           `json:"grants"`
	SSH       []aclRule           `json:"ssh"`
	Groups    map[string][]string `json:"groups"`
	TagOwners map[string][]string `json:"tagOwners"`
	Hosts     map[string]string   `json:"hosts"`
}

// aclRule covers the three rule dialects — legacy `acls`, modern `grants`,
// and `ssh` — which share src/dst and differ only in the trailing fields.
// One struct decodes all three; the unused fields stay zero.
type aclRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
	Proto  string   `json:"proto"`
	IP     []string `json:"ip"`    // grants: the port/protocol capabilities
	Users  []string `json:"users"` // ssh: the unix users that may be assumed
}

// collectACL fetches the tailnet policy file and explodes it into assets.
//
// Rules are emitted one asset apiece rather than buried in a single policy
// blob because they are the tailnet's traffic-flow definition: the topology
// layer joins their src/dst selectors to devices to draw who-can-reach-whom
// edges, which needs one addressable node per rule.
func (p *Provider) collectACL(ctx context.Context, out chan<- core.Asset) error {
	var pol aclPolicy
	if err := p.client.getJSON(ctx, p.tailnetPath("/acl"), &pol); err != nil {
		// A tailnet with no custom policy answers 404 rather than returning
		// the default document. That's "nothing configured", not a failure.
		if isNotFound(err) {
			return nil
		}
		return err
	}

	for _, a := range p.policyAssets(pol) {
		if !sendAsset(ctx, out, a) {
			return nil
		}
	}
	return nil
}

// policyAssets flattens a policy document into assets. Split out from the
// network call so it can be unit-tested against a literal policy.
//
// Map iteration order is randomised in Go, so group/tag-owner/host keys are
// sorted before emission — two audits of an unchanged tailnet must produce
// the same asset stream, or every downstream diff reports phantom drift.
func (p *Provider) policyAssets(pol aclPolicy) []core.Asset {
	var out []core.Asset

	out = append(out, p.policySummaryAsset(pol))

	for i, r := range pol.ACLs {
		out = append(out, p.ruleToAsset("acl", i, r))
	}
	for i, r := range pol.Grants {
		out = append(out, p.ruleToAsset("grant", i, r))
	}
	for i, r := range pol.SSH {
		out = append(out, p.ruleToAsset("ssh", i, r))
	}

	for _, name := range sortedKeys(pol.Groups) {
		out = append(out, p.groupToAsset(name, pol.Groups[name]))
	}
	for _, tag := range sortedKeys(pol.TagOwners) {
		out = append(out, p.tagOwnerToAsset(tag, pol.TagOwners[tag]))
	}
	for _, host := range sortedKeys(pol.Hosts) {
		out = append(out, p.hostToAsset(host, pol.Hosts[host]))
	}
	return out
}

func (p *Provider) policySummaryAsset(pol aclPolicy) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.acl",
		ID:        "acl:" + p.cfg.Tailnet,
		Name:      "Tailnet policy file",
		Status:    "active",
		Tags: tagsOf(
			"acl_rules", intStr(len(pol.ACLs)),
			"grants", intStr(len(pol.Grants)),
			"ssh_rules", intStr(len(pol.SSH)),
			"groups", intStr(len(pol.Groups)),
			"tag_owners", intStr(len(pol.TagOwners)),
			"hosts", intStr(len(pol.Hosts)),
		),
		Raw: p.rawOf(pol),
	}
}

// ruleToAsset maps one rule. Rules have no server-side identity, so the ID is
// synthesised from its dialect and ordinal — stable across runs as long as the
// policy file's rule order is unchanged, which is how the file is edited.
func (p *Provider) ruleToAsset(kind string, i int, r aclRule) core.Asset {
	action := r.Action
	if action == "" {
		// `grants` carry no action field — a grant is an allow by
		// definition (there is no deny form).
		action = "accept"
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.acl_rule",
		ID:        fmt.Sprintf("%s:%s/%s/%d", p.cfg.Tailnet, "acl", kind, i),
		Name:      fmt.Sprintf("%s %d: %s → %s", kind, i, joinStr(r.Src), joinStr(r.Dst)),
		Status:    action,
		Tags: tagsOf(
			"rule_kind", kind,
			"action", action,
			"src", joinStr(r.Src),
			"dst", joinStr(r.Dst),
			"proto", r.Proto,
			"capabilities", joinStr(r.IP),
			"ssh_users", joinStr(r.Users),
			"index", intStr(i),
		),
		Raw: p.rawOf(r),
	}
}

func (p *Provider) groupToAsset(name string, members []string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.acl_group",
		ID:        p.cfg.Tailnet + ":group/" + name,
		Name:      name,
		Tags: tagsOf(
			"members", joinStr(members),
			"member_count", intStr(len(members)),
		),
		Raw: p.rawOf(members),
	}
}

func (p *Provider) tagOwnerToAsset(tag string, owners []string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.acl_tag",
		ID:        p.cfg.Tailnet + ":tag/" + tag,
		Name:      tag,
		Tags: tagsOf(
			"owners", joinStr(owners),
			"owner_count", intStr(len(owners)),
		),
		Raw: p.rawOf(owners),
	}
}

// hostToAsset maps a policy `hosts` entry — a named IP or CIDR alias. The
// address is exposed as an "ip" tag so the topology index buckets it the same
// way a device address is, letting a DNS record or load balancer that points
// at the literal resolve to the named host.
func (p *Provider) hostToAsset(name, addr string) core.Asset {
	return core.Asset{
		Provider:  providerName,
		AccountID: p.cfg.Tailnet,
		Type:      "tailscale.acl_host",
		ID:        p.cfg.Tailnet + ":host/" + name,
		Name:      name,
		Tags: tagsOf(
			"ip", addr,
			"hostname", name,
		),
		Raw: p.rawOf(addr),
	}
}

// sortedKeys returns a map's keys in lexicographic order, so map-derived
// assets stream deterministically.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
