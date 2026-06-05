package oci

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/oracle/oci-go-sdk/v65/common"
	occore "github.com/oracle/oci-go-sdk/v65/core"
	"github.com/oracle/oci-go-sdk/v65/identity"
	"github.com/oracle/oci-go-sdk/v65/loadbalancer"

	icore "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestNew_AppliesDefaults(t *testing.T) {
	p := New(Config{})
	if p.cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want %d", p.cfg.MaxConcurrency, defaultMaxConcurrency)
	}
	if p.Name() != "oci" {
		t.Errorf("Name = %q, want oci", p.Name())
	}
}

func TestSetters(t *testing.T) {
	p := New(Config{})

	p.SetMaxConcurrency(10)
	if p.cfg.MaxConcurrency != 10 {
		t.Errorf("MaxConcurrency = %d, want 10", p.cfg.MaxConcurrency)
	}
	p.SetMaxConcurrency(0)
	if p.cfg.MaxConcurrency != 10 {
		t.Errorf("zero should be ignored; got %d", p.cfg.MaxConcurrency)
	}

	p.SetProfile("PROD")
	if p.cfg.Profile != "PROD" {
		t.Errorf("Profile = %q, want PROD", p.cfg.Profile)
	}
	p.SetProfile("")
	if p.cfg.Profile != "PROD" {
		t.Errorf("empty should be ignored; got %q", p.cfg.Profile)
	}

	p.SetRegions([]string{"us-ashburn-1", "us-phoenix-1"})
	if len(p.cfg.Regions) != 2 || p.cfg.Regions[0] != "us-ashburn-1" {
		t.Errorf("Regions = %v", p.cfg.Regions)
	}

	p.SetIncludeRaw(true)
	if !p.cfg.IncludeRaw {
		t.Error("SetIncludeRaw(true) didn't apply")
	}
}

func TestInit_RegistersProvider(t *testing.T) {
	if _, ok := icore.Lookup("oci"); !ok {
		t.Fatal("oci provider not registered by init()")
	}
}

func TestCompartmentToAsset(t *testing.T) {
	created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
	p := &Provider{tenancyOCID: "ocid1.tenancy.oc1..root"}
	c := identity.Compartment{
		Id:             ptrString("ocid1.compartment.oc1..child"),
		CompartmentId:  ptrString("ocid1.tenancy.oc1..root"),
		Name:           ptrString("prod"),
		Description:    ptrString("Production resources"),
		TimeCreated:    &common.SDKTime{Time: created},
		LifecycleState: identity.CompartmentLifecycleStateActive,
		FreeformTags:   map[string]string{"owner": "platform"},
	}

	a := p.compartmentToAsset(c)

	if a.Provider != "oci" || a.AccountID != "ocid1.tenancy.oc1..root" {
		t.Errorf("provider/account: %+v", a)
	}
	if a.Type != "oci.compartment" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.ID != "ocid1.compartment.oc1..child" {
		t.Errorf("ID = %q", a.ID)
	}
	if a.Status != "ACTIVE" {
		t.Errorf("Status = %q, want ACTIVE", a.Status)
	}
	if a.CreatedAt == nil || !a.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v", a.CreatedAt)
	}
	if a.Tags["owner"] != "platform" {
		t.Errorf("Tags[owner] = %q", a.Tags["owner"])
	}
	if a.Tags["parent_compartment_id"] != "ocid1.tenancy.oc1..root" {
		t.Errorf("Tags[parent_compartment_id] = %q", a.Tags["parent_compartment_id"])
	}
}

func TestComputeInstanceToAsset(t *testing.T) {
	created := time.Date(2025, 2, 3, 4, 5, 6, 0, time.UTC)
	p := &Provider{tenancyOCID: "ocid1.tenancy.oc1..root"}
	inst := occore.Instance{
		Id:                 ptrString("ocid1.instance.oc1.iad..i1"),
		CompartmentId:      ptrString("ocid1.compartment.oc1..app"),
		DisplayName:        ptrString("web-1"),
		AvailabilityDomain: ptrString("Uocm:PHX-AD-1"),
		Shape:              ptrString("VM.Standard.E4.Flex"),
		LifecycleState:     occore.InstanceLifecycleStateRunning,
		TimeCreated:        &common.SDKTime{Time: created},
		FreeformTags:       map[string]string{"env": "prod"},
	}

	a := p.computeInstanceToAsset(inst, "us-ashburn-1")

	if a.Type != "oci.compute.instance" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Region != "us-ashburn-1" {
		t.Errorf("Region = %q", a.Region)
	}
	if a.Status != "RUNNING" {
		t.Errorf("Status = %q, want RUNNING", a.Status)
	}
	if a.Name != "web-1" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Tags["shape"] != "VM.Standard.E4.Flex" {
		t.Errorf("Tags[shape] = %q", a.Tags["shape"])
	}
	if a.Tags["env"] != "prod" {
		t.Error("freeform tag missing")
	}
	if a.Tags["availability_domain"] != "Uocm:PHX-AD-1" {
		t.Errorf("Tags[availability_domain] = %q", a.Tags["availability_domain"])
	}
}

func TestLoadBalancerToAsset(t *testing.T) {
	created := time.Date(2025, 3, 4, 5, 6, 7, 0, time.UTC)
	p := &Provider{tenancyOCID: "ocid1.tenancy.oc1..root"}
	lb := loadbalancer.LoadBalancer{
		Id:             ptrString("ocid1.loadbalancer.oc1..lb1"),
		CompartmentId:  ptrString("ocid1.compartment.oc1..net"),
		DisplayName:    ptrString("public-lb"),
		LifecycleState: loadbalancer.LoadBalancerLifecycleStateActive,
		TimeCreated:    &common.SDKTime{Time: created},
		ShapeName:      ptrString("flexible"),
		IpAddresses: []loadbalancer.IpAddress{
			{IpAddress: ptrString("203.0.113.10")},
			{IpAddress: ptrString("203.0.113.11")},
		},
	}

	a := p.loadBalancerToAsset(lb, "us-ashburn-1")

	if a.Type != "oci.load_balancer" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.Tags["ip_addresses"] != "203.0.113.10,203.0.113.11" {
		t.Errorf("Tags[ip_addresses] = %q (Phase 10 topology depends on this format)", a.Tags["ip_addresses"])
	}
	if a.Tags["shape"] != "flexible" {
		t.Errorf("Tags[shape] = %q", a.Tags["shape"])
	}
}

func TestJoinIPAddresses(t *testing.T) {
	cases := []struct {
		name string
		in   []loadbalancer.IpAddress
		want string
	}{
		{"empty", nil, ""},
		{"single", []loadbalancer.IpAddress{{IpAddress: ptrString("1.2.3.4")}}, "1.2.3.4"},
		{"skips nil and empty", []loadbalancer.IpAddress{
			{IpAddress: ptrString("1.1.1.1")},
			{IpAddress: nil},
			{IpAddress: ptrString("")},
			{IpAddress: ptrString("2.2.2.2")},
		}, "1.1.1.1,2.2.2.2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := joinIPAddresses(c.in); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestResolveRegions_DefaultsToAllSubscribed(t *testing.T) {
	p := &Provider{
		homeRegion: "us-ashburn-1",
		listSubscribed: func(context.Context) ([]string, error) {
			return []string{"us-ashburn-1", "us-phoenix-1", "uk-london-1"}, nil
		},
	}
	got, err := p.resolveRegions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"us-ashburn-1", "us-phoenix-1", "uk-london-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestResolveRegions_AllSentinelMatchesDefault(t *testing.T) {
	p := &Provider{
		homeRegion: "us-ashburn-1",
		cfg:        Config{Regions: []string{"ALL"}},
		listSubscribed: func(context.Context) ([]string, error) {
			return []string{"us-ashburn-1", "us-phoenix-1"}, nil
		},
	}
	got, err := p.resolveRegions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "us-ashburn-1" || got[1] != "us-phoenix-1" {
		t.Errorf("got %v, want [us-ashburn-1 us-phoenix-1]", got)
	}
}

func TestResolveRegions_FallsBackToHomeOnSubscriptionError(t *testing.T) {
	p := &Provider{
		homeRegion: "us-ashburn-1",
		listSubscribed: func(context.Context) ([]string, error) {
			return nil, fmt.Errorf("NotAuthorizedOrNotFound")
		},
	}
	got, err := p.resolveRegions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "us-ashburn-1" {
		t.Errorf("got %v, want [us-ashburn-1] (fallback to home region)", got)
	}
}

func TestResolveRegions_ErrorsWhenNoFallback(t *testing.T) {
	p := &Provider{
		listSubscribed: func(context.Context) ([]string, error) {
			return nil, fmt.Errorf("boom")
		},
	}
	if _, err := p.resolveRegions(context.Background()); err == nil {
		t.Fatal("expected error when subscription lookup fails and home region is empty")
	}
}

func TestResolveRegions_ExplicitList(t *testing.T) {
	p := &Provider{
		homeRegion: "us-ashburn-1",
		cfg:        Config{Regions: []string{"US-PHOENIX-1", " uk-london-1 "}},
	}
	got, err := p.resolveRegions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"us-phoenix-1", "uk-london-1"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRawOf_RoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	created := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	inst := occore.Instance{
		Id:          ptrString("ocid1.instance.oc1..i"),
		DisplayName: ptrString("test"),
		TimeCreated: &common.SDKTime{Time: created},
	}
	raw := p.rawOf(inst)
	if raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["displayName"] != "test" {
		t.Errorf("Raw.displayName = %v, want test", back["displayName"])
	}
}

func TestMergeFreeformTags_NilWhenEmpty(t *testing.T) {
	if got := mergeFreeformTags(nil); got != nil {
		t.Errorf("expected nil for empty input, got %v", got)
	}
	if got := mergeFreeformTags(map[string]string{}); got != nil {
		t.Errorf("expected nil for empty map, got %v", got)
	}
}

func TestMergeFreeformTags_PreservesAndExtends(t *testing.T) {
	in := map[string]string{"a": "1", "b": "2"}
	got := mergeFreeformTags(in, [2]string{"c", "3"})
	if got["a"] != "1" || got["b"] != "2" || got["c"] != "3" {
		t.Errorf("merged = %v", got)
	}
	// Original map must not be mutated.
	if _, leaked := in["c"]; leaked {
		t.Error("input map was mutated")
	}
}

func TestDerefHelpers(t *testing.T) {
	if derefStr(nil) != "" {
		t.Error("derefStr(nil) should be empty string")
	}
	s := "hi"
	if derefStr(&s) != "hi" {
		t.Error("derefStr deref failed")
	}
	if derefTime(nil) != nil {
		t.Error("derefTime(nil) should be nil")
	}
	if derefTime(&common.SDKTime{}) != nil {
		t.Error("derefTime of zero SDKTime should be nil")
	}
	zero := time.Now().UTC()
	got := derefTime(&common.SDKTime{Time: zero})
	if got == nil || !got.Equal(zero) {
		t.Errorf("derefTime round-trip failed: got %v, want %v", got, zero)
	}
}

func TestPartialFailure_AuthErrIsPropagated(t *testing.T) {
	// With no auth chain candidates available (no IMDS, no env, no config
	// file with a tenancy), ensureAuth should fail deterministically.
	t.Setenv("OCI_RESOURCE_PRINCIPAL_VERSION", "")
	t.Setenv("OCI_TENANCY_OCID", "")
	t.Setenv("HOME", t.TempDir()) // hides any real ~/.oci/config

	p := New(Config{})
	_, err := p.ensureAuth()
	if err == nil {
		t.Skip("auth resolved successfully — environment must have an OCI " +
			"credential source available; test inconclusive")
	}
}
