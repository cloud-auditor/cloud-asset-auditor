package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// stubDiscovery overrides ServerPreferredResources, which the upstream fake
// hardcodes to (nil, nil) — useless for driving discovery paths.
type stubDiscovery struct {
	*fakediscovery.FakeDiscovery
	preferred []*metav1.APIResourceList
	err       error
}

func (s *stubDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return s.preferred, s.err
}

func newStubDiscovery(preferred []*metav1.APIResourceList, err error) *stubDiscovery {
	return &stubDiscovery{
		FakeDiscovery: &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}},
		preferred:     preferred,
		err:           err,
	}
}

var podsGVR = schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}

func podPreferredResources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{{
		GroupVersion: "v1",
		APIResources: []metav1.APIResource{
			{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: []string{"get", "list", "watch"}},
		},
	}}
}

func newPod(name, ns string) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "uid": name + "-uid"}
	if ns != "" {
		meta["namespace"] = ns
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata":   meta,
	}}
}

func newPodFakeDynamic(objs ...runtime.Object) *dynfake.FakeDynamicClient {
	return dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{podsGVR: "PodList"},
		objs...,
	)
}

// newTestProvider wires fakes directly, pre-firing the sync.Once so
// ensureClients never goes looking for a kubeconfig.
func newTestProvider(disc discovery.DiscoveryInterface, dyn dynamic.Interface, cfg Config) *Provider {
	p := New(cfg)
	p.clientOnce.Do(func() {})
	p.discovery = disc
	p.dynamic = dyn
	p.clusterID = "test-cluster"
	return p
}

// drain reads both Collect channels to completion, proving each closes.
func drain(t *testing.T, assets <-chan core.Asset, errs <-chan error) ([]core.Asset, []error) {
	t.Helper()
	var as []core.Asset
	var es []error
	timeout := time.After(10 * time.Second)
	for assets != nil || errs != nil {
		select {
		case a, ok := <-assets:
			if !ok {
				assets = nil
				continue
			}
			as = append(as, a)
		case e, ok := <-errs:
			if !ok {
				errs = nil
				continue
			}
			es = append(es, e)
		case <-timeout:
			t.Fatal("timed out draining Collect channels")
		}
	}
	return as, es
}

func TestCollect_StreamsAssets(t *testing.T) {
	dyn := newPodFakeDynamic(newPod("web", "default"), newPod("api", "prod"))
	p := newTestProvider(newStubDiscovery(podPreferredResources(), nil), dyn, Config{})

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
	if len(assets) != 2 {
		t.Fatalf("got %d assets, want 2: %v", len(assets), assets)
	}
	for _, a := range assets {
		if a.Provider != "kubernetes" || a.AccountID != "test-cluster" || a.Type != "v1.Pod" {
			t.Errorf("asset = %+v, want kubernetes/test-cluster/v1.Pod", a)
		}
	}
}

func TestCollect_PartialDiscoveryIsWarning(t *testing.T) {
	partial := &discovery.ErrGroupDiscoveryFailed{
		Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("the server is currently unable to handle the request"),
		},
	}
	dyn := newPodFakeDynamic(newPod("web", "default"))
	p := newTestProvider(newStubDiscovery(podPreferredResources(), partial), dyn, Config{})

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(assets) != 1 {
		t.Errorf("got %d assets, want 1 (audit must continue past partial discovery)", len(assets))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "partial") {
		t.Errorf("errs = %v, want one warning mentioning 'partial'", errs)
	}
}

func TestCollect_FatalDiscoveryError(t *testing.T) {
	p := newTestProvider(newStubDiscovery(nil, errors.New("connection refused")), nil, Config{})

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(assets) != 0 {
		t.Errorf("got %d assets, want 0", len(assets))
	}
	var sawCause, sawEmpty bool
	for _, e := range errs {
		if strings.Contains(e.Error(), "connection refused") {
			sawCause = true
		}
		if strings.Contains(e.Error(), "no listable resources") {
			sawEmpty = true
		}
	}
	if !sawCause || !sawEmpty {
		t.Errorf("errs = %v, want both the discovery cause and the no-resources error", errs)
	}
}

func TestCollect_NoListableResources(t *testing.T) {
	p := newTestProvider(newStubDiscovery(nil, nil), nil, Config{})

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(assets) != 0 {
		t.Errorf("got %d assets, want 0", len(assets))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "no listable resources") {
		t.Errorf("errs = %v, want exactly the no-listable-resources error", errs)
	}
}

func TestCollect_AuthErrorSurfaced(t *testing.T) {
	p := New(Config{})
	p.clientOnce.Do(func() { p.clientErr = errors.New("kubeconfig missing") })

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(assets) != 0 {
		t.Errorf("got %d assets, want 0", len(assets))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "kubernetes auth: kubeconfig missing") {
		t.Errorf("errs = %v, want single 'kubernetes auth' error", errs)
	}
}

func TestCollect_ListErrorRoutedToErrsChannel(t *testing.T) {
	dyn := newPodFakeDynamic()
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: request timed out")
	})
	p := newTestProvider(newStubDiscovery(podPreferredResources(), nil), dyn, Config{})

	assetsCh, errsCh := p.Collect(t.Context())
	assets, errs := drain(t, assetsCh, errsCh)
	if len(assets) != 0 {
		t.Errorf("got %d assets, want 0", len(assets))
	}
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "etcdserver: request timed out") {
		t.Errorf("errs = %v, want the list failure surfaced (not swallowed)", errs)
	}
}

func TestValidate_OK(t *testing.T) {
	p := newTestProvider(newStubDiscovery(nil, nil), nil, Config{})
	if err := p.Validate(t.Context()); err != nil {
		t.Fatalf("Validate = %v, want nil", err)
	}
}

func TestValidate_ServerVersionError(t *testing.T) {
	disc := newStubDiscovery(nil, nil)
	disc.PrependReactor("get", "version", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})
	p := newTestProvider(disc, nil, Config{})

	err := p.Validate(t.Context())
	if err == nil || !strings.Contains(err.Error(), "server version") {
		t.Fatalf("Validate = %v, want wrapped 'server version' error", err)
	}
}

func TestEmitErr(t *testing.T) {
	// Delivered when the channel has room.
	errs := make(chan error, 1)
	emitErr(context.Background(), errs, errors.New("boom"))
	select {
	case e := <-errs:
		if e.Error() != "boom" {
			t.Errorf("got %v", e)
		}
	default:
		t.Error("error was not delivered")
	}

	// Dropped (not deadlocked) when the channel is blocked and ctx is done.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	blocked := make(chan error) // unbuffered, nobody reading
	done := make(chan struct{})
	go func() {
		emitErr(ctx, blocked, errors.New("lost"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emitErr hung on a blocked channel with cancelled context")
	}
}
