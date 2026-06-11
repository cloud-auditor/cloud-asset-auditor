package kubernetes

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// listOnce runs listResource for one target and collects what it emitted.
func listOnce(t *testing.T, p *Provider, target resourceTarget) ([]core.Asset, error) {
	t.Helper()
	out := make(chan core.Asset, 64)
	err := p.listResource(t.Context(), target, out)
	close(out)
	var got []core.Asset
	for a := range out {
		got = append(got, a)
	}
	return got, err
}

func TestListResource_ForbiddenSwallowed(t *testing.T) {
	dyn := newPodFakeDynamic()
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", errors.New("RBAC: access denied"))
	})
	p := newTestProvider(nil, dyn, Config{})

	got, err := listOnce(t, p, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"})
	if err != nil {
		t.Fatalf("Forbidden must be swallowed (permission gap, not a bug); got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d assets, want 0", len(got))
	}
}

func TestListResource_MethodNotSupportedSwallowed(t *testing.T) {
	dyn := newPodFakeDynamic()
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewMethodNotSupported(
			schema.GroupResource{Resource: "pods"}, "list")
	})
	p := newTestProvider(nil, dyn, Config{})

	got, err := listOnce(t, p, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"})
	if err != nil {
		t.Fatalf("MethodNotSupported must be swallowed; got %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d assets, want 0", len(got))
	}
}

func TestListResource_OtherErrorSurfaced(t *testing.T) {
	dyn := newPodFakeDynamic()
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("etcdserver: leader changed")
	})
	p := newTestProvider(nil, dyn, Config{})

	_, err := listOnce(t, p, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"})
	if err == nil {
		t.Fatal("non-permission errors must be surfaced")
	}
	if !strings.Contains(err.Error(), "list") || !strings.Contains(err.Error(), "etcdserver: leader changed") {
		t.Errorf("error = %v, want wrapped list error with cause", err)
	}
}

func TestListResource_NamespaceScopedSkipsExclusions(t *testing.T) {
	dyn := newPodFakeDynamic(newPod("a", "prod"), newPod("b", "default"))
	// When scoped to a single namespace the API server already filtered;
	// --kube-exclude-namespaces must NOT apply (even if it names that very
	// namespace).
	p := newTestProvider(nil, dyn, Config{
		KubeNamespace:         "prod",
		KubeExcludeNamespaces: []string{"prod"},
	})

	got, err := listOnce(t, p, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "a" {
		t.Errorf("got %v, want exactly pod 'a' from namespace prod", got)
	}
}

func TestListResource_ClusterScoped(t *testing.T) {
	nodesGVR := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "nodes"}
	node := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "Node",
		"metadata":   map[string]any{"name": "node-1", "uid": "node-1-uid"},
	}}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{nodesGVR: "NodeList"},
		node,
	)
	// Default exclusions present; they must not affect cluster-scoped kinds.
	p := newTestProvider(nil, dyn, Config{})

	got, err := listOnce(t, p, resourceTarget{GVR: nodesGVR, Namespaced: false, Kind: "Node"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "node-1" {
		t.Fatalf("got %v, want node-1", got)
	}
	if _, ok := got[0].Tags["namespace"]; ok {
		t.Errorf("cluster-scoped asset must not carry a namespace tag: %v", got[0].Tags)
	}
}

func TestListResource_PaginatesOnContinueToken(t *testing.T) {
	dyn := newPodFakeDynamic()
	calls := 0
	dyn.PrependReactor("list", "pods", func(clienttesting.Action) (bool, runtime.Object, error) {
		calls++
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion("v1")
		list.SetKind("PodList")
		if calls == 1 {
			list.SetContinue("page-2")
			list.Items = []unstructured.Unstructured{*newPod("first", "default")}
		} else {
			list.Items = []unstructured.Unstructured{*newPod("second", "default")}
		}
		return true, list, nil
	})
	p := newTestProvider(nil, dyn, Config{})

	got, err := listOnce(t, p, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("List called %d times, want 2 (continue token must drive a second page)", calls)
	}
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Errorf("got %v, want [first second] across two pages", got)
	}
}

func TestListResource_StopsOnCancelledContext(t *testing.T) {
	dyn := newPodFakeDynamic(newPod("a", "default"))
	p := newTestProvider(nil, dyn, Config{})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := make(chan core.Asset) // unbuffered and unread: only ctx.Done can unblock the send
	err := p.listResource(ctx, resourceTarget{GVR: podsGVR, Namespaced: true, Kind: "Pod"}, out)
	if err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want nil or context.Canceled", err)
	}
}

func TestSendAsset_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := make(chan core.Asset) // unbuffered, no reader
	if sendAsset(ctx, out, core.Asset{}) {
		t.Error("sendAsset must report false when the context is done")
	}
}
