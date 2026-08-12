package gcp

import "testing"

func TestResourceToAsset(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	a := p.resourceToAsset(resource{
		Name:        "//compute.googleapis.com/projects/p/zones/us-central1-a/instances/web-1",
		AssetType:   "compute.googleapis.com/Instance",
		Project:     "projects/123456",
		DisplayName: "web-1",
		Location:    "us-central1-a",
		State:       "RUNNING",
		Labels:      map[string]string{"env": "prod"},
		NetworkTags: []string{"http", "https"},
		CreateTime:  "2023-01-02T03:04:05Z",
	})
	if a.Provider != "gcp" || a.Type != "compute.googleapis.com/Instance" {
		t.Fatalf("bad type/provider: %+v", a)
	}
	if a.ID != "//compute.googleapis.com/projects/p/zones/us-central1-a/instances/web-1" {
		t.Errorf("ID should be the full resource name, got %q", a.ID)
	}
	if a.Name != "web-1" || a.Region != "us-central1-a" || a.Status != "RUNNING" {
		t.Errorf("name/region/status wrong: %+v", a)
	}
	if a.AccountID != "123456" {
		t.Errorf("AccountID = %q, want 123456 (project number, projects/ stripped)", a.AccountID)
	}
	if a.Tags["env"] != "prod" || a.Tags["network_tags"] != "http,https" {
		t.Errorf("tags wrong: %v", a.Tags)
	}
	if a.CreatedAt == nil {
		t.Error("CreatedAt should parse from createTime")
	}
	if len(a.Raw) == 0 {
		t.Error("Raw should be set with IncludeRaw")
	}
}

func TestResourceToAsset_DisplayNameFallsBackToShortName(t *testing.T) {
	p := &Provider{}
	a := p.resourceToAsset(resource{
		Name:      "//storage.googleapis.com/projects/p/buckets/my-bucket",
		AssetType: "storage.googleapis.com/Bucket",
	})
	if a.Name != "my-bucket" {
		t.Errorf("Name = %q, want my-bucket (last path segment)", a.Name)
	}
	if a.Raw != nil {
		t.Error("Raw must be nil without IncludeRaw")
	}
}

func TestBuildTags_LabelsWinOnCollision(t *testing.T) {
	got := buildTags(resource{
		Labels:      map[string]string{"description": "label-value"},
		Description: "resource-description",
		NetworkTags: []string{"a"},
	})
	if got["description"] != "label-value" {
		t.Errorf("a real label must win over the GCP-derived extra, got %q", got["description"])
	}
	if got["network_tags"] != "a" {
		t.Errorf("network_tags = %q", got["network_tags"])
	}
}

func TestScopeFromEnv(t *testing.T) {
	t.Setenv("GCP_SCOPE", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "my-proj")
	if got := scopeFromEnv(); got != "projects/my-proj" {
		t.Errorf("scopeFromEnv = %q, want projects/my-proj", got)
	}
	t.Setenv("GCP_SCOPE", "organizations/9")
	if got := scopeFromEnv(); got != "organizations/9" {
		t.Errorf("GCP_SCOPE should win, got %q", got)
	}
}

func TestValidateScope(t *testing.T) {
	for _, ok := range []string{"projects/my-proj", "projects/123456", "folders/123", "organizations/456", ""} {
		if err := validateScope(ok); err != nil {
			t.Errorf("validateScope(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"projects/a?b", "projects/a#b", "projects/a/b", "projects/a:x", "bogus/1", "projects/", "../evil", "projects/a b"} {
		if err := validateScope(bad); err == nil {
			t.Errorf("validateScope(%q) = nil, want an error", bad)
		}
	}
}

func TestQuotaProject(t *testing.T) {
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "")
	if got := (&Provider{cfg: Config{Scope: "projects/my-proj"}}).quotaProject(); got != "my-proj" {
		t.Errorf("projects scope quota = %q, want my-proj", got)
	}
	org := &Provider{cfg: Config{Scope: "organizations/123"}}
	if got := org.quotaProject(); got != "" {
		t.Errorf("org scope has no implicit quota project, got %q", got)
	}
	t.Setenv("GOOGLE_CLOUD_QUOTA_PROJECT", "billing-proj")
	if got := org.quotaProject(); got != "billing-proj" {
		t.Errorf("env override = %q, want billing-proj", got)
	}
}

func TestResolveScopeOverride(t *testing.T) {
	p := &Provider{cfg: Config{Scope: "projects/default"}}
	p.SetScope("") // empty must not clobber the default
	if p.cfg.Scope != "projects/default" {
		t.Errorf("empty SetScope clobbered the default: %q", p.cfg.Scope)
	}
	p.SetScope("organizations/42")
	if p.cfg.Scope != "organizations/42" {
		t.Errorf("SetScope override failed: %q", p.cfg.Scope)
	}
}

func TestBuildTags_ExtractsAddressesFromAdditionalAttributes(t *testing.T) {
	// Shaped like a real ForwardingRule search result: the address sits under
	// a key we deliberately do not hard-code, alongside strings that must not
	// be mistaken for one.
	tags := buildTags(resource{
		Name:        "//compute.googleapis.com/projects/p/global/forwardingRules/fe",
		Description: "front end 10 of 12",
		AdditionalAttributes: []byte(`{
			"ipAddress": "198.51.100.20",
			"portRange": "443-443",
			"target": "//compute.googleapis.com/projects/p/global/targetHttpsProxies/x",
			"nested": {"natIP": ["203.0.113.7", "not-an-ip"]}
		}`),
	})
	// Sorted and de-duplicated, so two runs over the same resource cannot
	// produce different tags and make `auditor diff` report phantom drift.
	if got, want := tags["ip_addresses"], "198.51.100.20,203.0.113.7"; got != want {
		t.Errorf("ip_addresses = %q, want %q", got, want)
	}
}

func TestBuildTags_NoAddressesLeavesTagAbsent(t *testing.T) {
	tags := buildTags(resource{
		Name:                 "//storage.googleapis.com/buckets/b",
		AdditionalAttributes: []byte(`{"locationType": "region"}`),
	})
	if _, ok := tags["ip_addresses"]; ok {
		t.Errorf("a resource with no address must not carry an empty ip_addresses tag, got %+v", tags)
	}
}
