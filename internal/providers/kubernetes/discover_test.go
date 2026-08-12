package kubernetes

import (
	"errors"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
)

func TestDiscoverResources_AuthErrorPropagates(t *testing.T) {
	p := New(Config{})
	p.clientOnce.Do(func() { p.clientErr = errors.New("no kubeconfig anywhere") })

	if _, err := p.discoverResources(); err == nil || !strings.Contains(err.Error(), "no kubeconfig anywhere") {
		t.Fatalf("err = %v, want the memoized client error", err)
	}
}

func TestDiscoverResources_PartialErrorReturnsTargets(t *testing.T) {
	partial := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("service unavailable"),
		},
	}
	p := newTestProvider(newStubDiscovery(podPreferredResources(), partial), nil, Config{})

	targets, err := p.discoverResources()
	var asPartial *discovery.ErrGroupDiscoveryFailed
	if !errors.As(err, &asPartial) {
		t.Fatalf("err = %v, want the partial-discovery error passed through", err)
	}
	if len(targets) != 1 || targets[0].GVR.Resource != "pods" {
		t.Errorf("targets = %v, want the pods target despite the partial error", targets)
	}
}

func TestDiscoverResources_FatalError(t *testing.T) {
	p := newTestProvider(newStubDiscovery(nil, errors.New("dial tcp: connection refused")), nil, Config{})

	targets, err := p.discoverResources()
	if err == nil || !strings.Contains(err.Error(), "discover preferred resources") {
		t.Fatalf("err = %v, want wrapped fatal discovery error", err)
	}
	if targets != nil {
		t.Errorf("targets = %v, want nil on fatal discovery error", targets)
	}
}

func TestDiscoverResources_Success(t *testing.T) {
	p := newTestProvider(newStubDiscovery(podPreferredResources(), nil), nil, Config{})

	targets, err := p.discoverResources()
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Kind != "Pod" || !targets[0].Namespaced {
		t.Errorf("targets = %v, want one namespaced Pod target", targets)
	}
}

func TestFilterResources_SkipsInvalidGroupVersion(t *testing.T) {
	in := []*metav1.APIResourceList{
		{
			GroupVersion: "this/is/not-a-groupversion", // unparseable → whole list skipped
			APIResources: []metav1.APIResource{
				{Name: "ghosts", Namespaced: true, Kind: "Ghost", Verbs: []string{"list"}},
			},
		},
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: []string{"list"}},
			},
		},
	}
	got := filterResources(in, false)
	if len(got) != 1 || got[0].GVR.Resource != "pods" {
		t.Fatalf("got %v, want only pods (invalid GroupVersion dropped)", got)
	}
}

func TestFilterResources_EmptyAndDegenerateInputs(t *testing.T) {
	if got := filterResources(nil, false); len(got) != 0 {
		t.Errorf("nil input → %v, want empty", got)
	}
	in := []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: nil}, // group with no resources
		{
			GroupVersion: "", // empty GroupVersion parses to the bare core group
			APIResources: []metav1.APIResource{
				{Name: "things", Namespaced: false, Kind: "Thing", Verbs: []string{"list"}},
			},
		},
		{
			GroupVersion: "batch/v1",
			APIResources: []metav1.APIResource{
				{Name: "jobs", Namespaced: true, Kind: "Job", Verbs: []string{"LIST"}}, // mixed-case verb still counts
				{Name: "jobs/status", Namespaced: true, Kind: "Job", Verbs: []string{"list"}},
				{Name: "cronjobs", Namespaced: true, Kind: "CronJob", Verbs: []string{"get", "watch"}},
			},
		},
	}
	got := filterResources(in, false)
	if len(got) != 2 {
		t.Fatalf("got %d targets, want 2 (things + jobs): %v", len(got), got)
	}
	var resources []string
	for _, target := range got {
		resources = append(resources, target.GVR.Resource)
	}
	for _, want := range []string{"things", "jobs"} {
		found := false
		for _, r := range resources {
			if r == want {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %q in %v", want, resources)
		}
	}
}

// Events are one stored object served through two API groups. Listing both
// collects every event twice under the same UID, which double-counts the
// inventory and collides on the (provider, id) identity that diff, the
// topology index, and the cache all key on.
func TestFilterResources_EventsAreCollectedOnce(t *testing.T) {
	lists := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "events", Kind: "Event", Namespaced: true, Verbs: []string{"list"}},
				{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: []string{"list"}},
			},
		},
		{
			GroupVersion: "events.k8s.io/v1",
			APIResources: []metav1.APIResource{
				{Name: "events", Kind: "Event", Namespaced: true, Verbs: []string{"list"}},
			},
		},
	}

	got := filterResources(lists, false)
	var events []string
	for _, t := range got {
		if t.GVR.Resource == "events" {
			events = append(events, t.GVR.String())
		}
	}
	if len(events) != 1 {
		t.Fatalf("expected exactly one events GVR, got %v", events)
	}
	if events[0] != "/v1, Resource=events" {
		t.Errorf("kept %q, want the core group — it is the one every cluster serves", events[0])
	}

	// A CRD that happens to be called "events" in someone else's group is a
	// different resource entirely and must survive.
	withCRD := append(lists, &metav1.APIResourceList{
		GroupVersion: "example.com/v1",
		APIResources: []metav1.APIResource{
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: []string{"list"}},
		},
	})
	var kept int
	for _, tgt := range filterResources(withCRD, false) {
		if tgt.GVR.Resource == "events" {
			kept++
		}
	}
	if kept != 2 {
		t.Errorf("core event + unrelated CRD should both survive, got %d", kept)
	}

	if n := len(filterResources(lists, true)); n != 1 {
		t.Errorf("--kube-exclude-events must still drop both groups, got %d targets", n)
	}
}
