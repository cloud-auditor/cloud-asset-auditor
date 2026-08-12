package core

import "testing"

// AsRef is what every topology edge endpoint is built from, so a change to
// which fields it carries silently reshapes every rendered graph and the
// /api/v1/topology JSON. Raw and Tags must stay out — a ref is an identity,
// not a copy of the asset.
func TestAsset_AsRef(t *testing.T) {
	a := Asset{
		Provider:  "oci",
		AccountID: "ocid1.tenancy.oc1..t",
		Region:    "eu-frankfurt-1",
		Type:      "oci.load_balancer",
		ID:        "ocid1.loadbalancer.oc1..lb",
		Name:      "prod-lb",
		Status:    "ACTIVE",
		Tags:      map[string]string{"ip_addresses": "203.0.113.10"},
		Raw:       []byte(`{"big":"payload"}`),
	}
	want := AssetRef{
		Provider:  "oci",
		AccountID: "ocid1.tenancy.oc1..t",
		Type:      "oci.load_balancer",
		ID:        "ocid1.loadbalancer.oc1..lb",
	}
	if got := a.AsRef(); got != want {
		t.Errorf("AsRef() = %+v, want %+v", got, want)
	}
}

// Policy engines spell the verdict differently; TrafficEdgeKind is the single
// place that normalises them. Getting this wrong draws a firewall's denials as
// reachability — the exact opposite of what the rule says — so the unknown and
// empty cases matter as much as the known ones.
func TestTrafficEdgeKind(t *testing.T) {
	for _, tc := range []struct{ action, want string }{
		{"accept", EdgeKindTrafficAllow},
		{"allow", EdgeKindTrafficAllow},
		{"check", EdgeKindTrafficAllow}, // Tailscale SSH: an allow gated on re-auth
		{"", EdgeKindTrafficAllow},      // NetBird grants carry no action field
		{"drop", EdgeKindTrafficDeny},
		{"deny", EdgeKindTrafficDeny},
		{"reject", EdgeKindTrafficDeny},
		// An unrecognised verdict defaults to allow, matching the "a rule that
		// exists but whose action we can't parse is more likely a grant"
		// reading of every policy language we map.
		{"something-new", EdgeKindTrafficAllow},
	} {
		if got := TrafficEdgeKind(tc.action); got != tc.want {
			t.Errorf("TrafficEdgeKind(%q) = %q, want %q", tc.action, got, tc.want)
		}
	}
}
