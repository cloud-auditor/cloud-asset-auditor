package gcp

import (
	"encoding/json"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// resource is the subset of a Cloud Asset Inventory ResourceSearchResult we
// map. Raw is a re-marshal of this struct when --include-raw is set, so a
// field that isn't declared here is not recoverable downstream.
type resource struct {
	Name                   string            `json:"name"`      // full resource name (unique)
	AssetType              string            `json:"assetType"` // e.g. compute.googleapis.com/Instance
	Project                string            `json:"project"`   // projects/<number>
	Folders                []string          `json:"folders"`
	Organization           string            `json:"organization"`
	DisplayName            string            `json:"displayName"`
	Description            string            `json:"description"`
	Location               string            `json:"location"`
	Labels                 map[string]string `json:"labels"`
	NetworkTags            []string          `json:"networkTags"`
	KMSKeys                []string          `json:"kmsKeys"`
	State                  string            `json:"state"`
	CreateTime             string            `json:"createTime"`
	UpdateTime             string            `json:"updateTime"`
	ParentFullResourceName string            `json:"parentFullResourceName"`
	ParentAssetType        string            `json:"parentAssetType"`

	// AdditionalAttributes is a free-form google.protobuf.Struct whose keys
	// differ per asset type — IPAddress on a ForwardingRule, natIP inside an
	// Instance's access configs, and so on. It is where a resource's
	// addresses live, which is what makes it worth carrying.
	AdditionalAttributes json.RawMessage `json:"additionalAttributes,omitempty"`
}

func (p *Provider) resourceToAsset(r resource) core.Asset {
	name := r.DisplayName
	if name == "" {
		name = shortName(r.Name)
	}
	return core.Asset{
		Provider:  providerName,
		AccountID: strings.TrimPrefix(r.Project, "projects/"),
		Region:    r.Location,
		Type:      r.AssetType,
		ID:        r.Name,
		Name:      name,
		Status:    r.State,
		CreatedAt: parseTime(r.CreateTime),
		Tags:      buildTags(r),
		Raw:       p.rawOf(r),
	}
}

// buildTags merges the resource's labels with a few useful flattened fields.
// Labels win on key collision (a GCP-derived extra never clobbers a real label).
func buildTags(r resource) map[string]string {
	tags := make(map[string]string, len(r.Labels)+4)
	for k, v := range r.Labels {
		tags[k] = v
	}
	add := func(k, v string) {
		if v == "" {
			return
		}
		if _, exists := tags[k]; !exists {
			tags[k] = v
		}
	}
	add("network_tags", strings.Join(r.NetworkTags, ","))
	add("description", r.Description)
	add("organization", r.Organization)
	add("parent_asset_type", r.ParentAssetType)
	add("folders", strings.Join(r.Folders, ","))
	add("kms_keys", strings.Join(r.KMSKeys, ","))
	// Same key and same comma-joined format the OCI load balancer uses, so
	// the topology index can join a DNS record to a GCP address without a
	// second vocabulary — and without needing --include-raw, which is the
	// whole point (a plain audit snapshot still resolves the edge).
	add("ip_addresses", strings.Join(resourceAddresses(r), ","))
	if len(tags) == 0 {
		return nil
	}
	return tags
}

// resourceAddresses extracts the IP literals a resource publishes, from the
// free-form additionalAttributes blob.
//
// It walks every value and keeps the ones net.ParseIP accepts, rather than
// reading a table of known keys. The key names differ per asset type and GCP
// adds asset types continuously, so a key table is stale the day it is
// written; an IP-shaped string sitting in a resource's own attributes is that
// resource's address in practice. net.ParseIP is a hard filter — a label, a
// description, or a resource name cannot pass it by accident.
//
// The result is sorted so two runs over the same resource produce identical
// tags (map iteration order would otherwise make `auditor diff` report drift
// that isn't there).
func resourceAddresses(r resource) []string {
	if len(r.AdditionalAttributes) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(r.AdditionalAttributes, &v); err != nil {
		return nil
	}
	seen := map[string]struct{}{}
	collectIPs(v, seen)
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for ip := range seen {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

// collectIPs walks a decoded JSON value, adding every string that parses as an
// IP address. Depth is bounded by the payload itself, which the API caps well
// below anything that could exhaust the stack.
func collectIPs(v any, out map[string]struct{}) {
	switch t := v.(type) {
	case string:
		if net.ParseIP(t) != nil {
			out[t] = struct{}{}
		}
	case []any:
		for _, e := range t {
			collectIPs(e, out)
		}
	case map[string]any:
		for _, e := range t {
			collectIPs(e, out)
		}
	}
}

// shortName returns the last path segment of a full resource name, used as the
// display name when the API didn't supply one.
func shortName(fullName string) string {
	if i := strings.LastIndex(fullName, "/"); i >= 0 && i < len(fullName)-1 {
		return fullName[i+1:]
	}
	return fullName
}

func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}
