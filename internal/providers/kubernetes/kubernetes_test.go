package kubernetes

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestNew_AppliesDefaults(t *testing.T) {
	p := New(Config{})
	if p.cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want %d", p.cfg.MaxConcurrency, defaultMaxConcurrency)
	}
	if len(p.cfg.KubeExcludeNamespaces) != 3 {
		t.Errorf("default exclusion list len = %d, want 3", len(p.cfg.KubeExcludeNamespaces))
	}
	if p.Name() != "kubernetes" {
		t.Errorf("Name() = %q", p.Name())
	}
}

func TestSetters(t *testing.T) {
	p := New(Config{})

	p.SetMaxConcurrency(7)
	p.SetMaxConcurrency(0) // ignored
	if p.cfg.MaxConcurrency != 7 {
		t.Errorf("MaxConcurrency = %d, want 7", p.cfg.MaxConcurrency)
	}

	p.SetKubeContext("prod")
	p.SetKubeContext("") // ignored
	if p.cfg.KubeContext != "prod" {
		t.Errorf("KubeContext = %q", p.cfg.KubeContext)
	}

	p.SetKubeNamespace("apps")
	if p.cfg.KubeNamespace != "apps" {
		t.Errorf("KubeNamespace = %q", p.cfg.KubeNamespace)
	}

	custom := []string{"weird"}
	p.SetKubeExcludeNamespaces(custom)
	if len(p.cfg.KubeExcludeNamespaces) != 1 || p.cfg.KubeExcludeNamespaces[0] != "weird" {
		t.Errorf("ExcludeNamespaces = %v", p.cfg.KubeExcludeNamespaces)
	}
	// nil from "user didn't pass the flag" must NOT blank out the previously-set value.
	p.SetKubeExcludeNamespaces(nil)
	if len(p.cfg.KubeExcludeNamespaces) != 1 {
		t.Errorf("nil should not clobber; got %v", p.cfg.KubeExcludeNamespaces)
	}

	p.SetKubeExcludeHelmSecrets(true)
	if !p.cfg.ExcludeHelmSecrets {
		t.Error("ExcludeHelmSecrets not set")
	}
}

func TestIsHelmReleaseSecret(t *testing.T) {
	secret := func(name, typ string) *unstructured.Unstructured {
		o := map[string]any{"apiVersion": "v1", "kind": "Secret",
			"metadata": map[string]any{"name": name}}
		if typ != "" {
			o["type"] = typ
		}
		return &unstructured.Unstructured{Object: o}
	}
	cases := []struct {
		name string
		u    *unstructured.Unstructured
		want bool
	}{
		{"helm by type", secret("sh.helm.release.v1.argocd.v3", "helm.sh/release.v1"), true},
		{"helm by name only", secret("sh.helm.release.v1.loki.v2", ""), true},
		{"opaque secret", secret("argocd-secret", "Opaque"), false},
		{"tls secret", secret("argocd-tls", "kubernetes.io/tls"), false},
		{"non-secret named like helm", &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": "sh.helm.release.v1.x.v1"}}}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHelmReleaseSecret(c.u); got != c.want {
				t.Errorf("isHelmReleaseSecret = %v, want %v", got, c.want)
			}
		})
	}
}

func TestListResource_ExcludesHelmSecrets(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "secrets"}
	secret := func(name, typ string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Secret",
			"type":       typ,
			"metadata":   map[string]any{"name": name, "namespace": "argocd", "uid": name + "-uid"},
		}}
	}
	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{gvr: "SecretList"}
	objs := []runtime.Object{
		secret("sh.helm.release.v1.argocd.v1", "helm.sh/release.v1"),
		secret("argocd-secret", "Opaque"),
	}
	dynClient := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)

	p := &Provider{dynamic: dynClient, clusterID: "test", cfg: Config{ExcludeHelmSecrets: true}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make(chan core.Asset, 16)
	if err := p.listResource(ctx, resourceTarget{GVR: gvr, Namespaced: true, Kind: "Secret"}, out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var names []string
	for a := range out {
		names = append(names, a.Name)
	}
	if len(names) != 1 || names[0] != "argocd-secret" {
		t.Fatalf("got %v, want [argocd-secret] (helm release secret excluded)", names)
	}
}

func TestFormatType(t *testing.T) {
	cases := map[string]struct{ apiVersion, kind, want string }{
		"core group":       {"v1", "Pod", "v1.Pod"},
		"named group":      {"apps/v1", "Deployment", "apps/v1.Deployment"},
		"crd":              {"example.com/v1", "Widget", "example.com/v1.Widget"},
		"empty apiVersion": {"", "Node", "Node"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := formatType(c.apiVersion, c.kind); got != c.want {
				t.Errorf("formatType(%q,%q) = %q, want %q", c.apiVersion, c.kind, got, c.want)
			}
		})
	}
}

func TestIsSubresourceAndSupportsList(t *testing.T) {
	if !isSubresource("pods/status") {
		t.Error("pods/status should be a subresource")
	}
	if isSubresource("pods") {
		t.Error("pods should not be a subresource")
	}
	if !supportsList([]string{"get", "list", "watch"}) {
		t.Error("expected supports list")
	}
	if supportsList([]string{"get", "create"}) {
		t.Error("expected does not support list")
	}
}

func TestExtractStatus_Pod(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{"phase": "Running"},
	}}
	if got := extractStatus(u); got != "Running" {
		t.Errorf("status = %q, want Running", got)
	}
}

func TestExtractStatus_PrefersReadyCondition(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True"},
				map[string]any{"type": "Ready", "status": "False"},
			},
		},
	}}
	if got := extractStatus(u); got != "Ready=False" {
		t.Errorf("status = %q, want Ready=False (Ready beats Available)", got)
	}
}

func TestExtractStatus_NoStatus(t *testing.T) {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	if got := extractStatus(u); got != "" {
		t.Errorf("status should be empty, got %q", got)
	}
}

func TestCollapseTags(t *testing.T) {
	if collapseTags(nil, "") != nil {
		t.Error("empty input should produce nil tags")
	}
	got := collapseTags(map[string]string{"app": "web"}, "prod")
	if got["app"] != "web" || got["namespace"] != "prod" {
		t.Errorf("collapsed tags = %v", got)
	}
}

func TestUnstructuredToAsset(t *testing.T) {
	created := metav1.Time{Time: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}
	p := &Provider{clusterID: "kind-test"}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":              "web",
			"namespace":         "prod",
			"uid":               "11111111-2222-3333-4444-555555555555",
			"creationTimestamp": created.UTC().Format(time.RFC3339),
			"labels":            map[string]any{"app": "web", "tier": "frontend"},
		},
		"status": map[string]any{
			"conditions": []any{
				map[string]any{"type": "Available", "status": "True"},
			},
		},
	}}

	a := p.unstructuredToAsset(u)
	if a.Provider != "kubernetes" {
		t.Errorf("Provider = %q", a.Provider)
	}
	if a.AccountID != "kind-test" {
		t.Errorf("AccountID = %q", a.AccountID)
	}
	if a.Type != "apps/v1.Deployment" {
		t.Errorf("Type = %q", a.Type)
	}
	if a.ID != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("ID = %q", a.ID)
	}
	if a.Name != "web" {
		t.Errorf("Name = %q", a.Name)
	}
	if a.Tags["namespace"] != "prod" {
		t.Errorf("Tags[namespace] = %q", a.Tags["namespace"])
	}
	if a.Tags["app"] != "web" {
		t.Errorf("Tags[app] = %q", a.Tags["app"])
	}
	if a.Status != "Available=True" {
		t.Errorf("Status = %q, want Available=True", a.Status)
	}
}

func TestFilterResources(t *testing.T) {
	in := []*metav1.APIResourceList{
		{
			GroupVersion: "v1",
			APIResources: []metav1.APIResource{
				{Name: "pods", Namespaced: true, Kind: "Pod", Verbs: []string{"get", "list"}},
				{Name: "pods/status", Namespaced: true, Kind: "Pod", Verbs: []string{"get", "list"}}, // subresource → drop
				{Name: "nodes", Namespaced: false, Kind: "Node", Verbs: []string{"get"}},             // no list → drop
				{Name: "services", Namespaced: true, Kind: "Service", Verbs: []string{"list", "watch"}},
			},
		},
		{
			GroupVersion: "apps/v1",
			APIResources: []metav1.APIResource{
				{Name: "deployments", Namespaced: true, Kind: "Deployment", Verbs: []string{"list"}},
				{Name: "deployments/scale", Namespaced: true, Kind: "Scale", Verbs: []string{"list"}}, // subresource → drop
			},
		},
		{
			GroupVersion: "example.com/v1",
			APIResources: []metav1.APIResource{
				{Name: "widgets", Namespaced: true, Kind: "Widget", Verbs: []string{"list"}}, // CRD comes for free
			},
		},
	}
	got := filterResources(in)
	// Sort is by GVR.String(); core-group resources render as "/v1, Resource=..."
	// and "/" (ASCII 47) sorts before letters.
	want := []string{
		"/v1, Resource=pods",
		"/v1, Resource=services",
		"apps/v1, Resource=deployments",
		"example.com/v1, Resource=widgets",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d targets, want %d: %v", len(got), len(want), got)
	}
	for i, t2 := range got {
		if t2.GVR.String() != want[i] {
			t.Errorf("[%d] %q, want %q", i, t2.GVR.String(), want[i])
		}
	}
}

func TestListResource_FiltersExcludedNamespaces(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "", Version: "v1", Resource: "pods"}
	pod := func(name, ns string) *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1",
			"kind":       "Pod",
			"metadata": map[string]any{
				"name":      name,
				"namespace": ns,
				"uid":       name + "-uid",
			},
		}}
	}

	scheme := runtime.NewScheme()
	listKinds := map[schema.GroupVersionResource]string{gvr: "PodList"}
	objs := []runtime.Object{
		pod("a", "default"),
		pod("kube-stuff", "kube-system"),
		pod("b", "prod"),
	}
	dynClient := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds, objs...)

	p := &Provider{
		dynamic:   dynClient,
		clusterID: "test",
		cfg:       Config{KubeExcludeNamespaces: []string{"kube-system"}},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out := make(chan core.Asset, 16)
	if err := p.listResource(ctx, resourceTarget{GVR: gvr, Namespaced: true, Kind: "Pod"}, out); err != nil {
		t.Fatal(err)
	}
	close(out)

	var names []string
	for a := range out {
		names = append(names, a.Name)
	}
	// Should see "a" and "b", but NOT "kube-stuff".
	if len(names) != 2 {
		t.Fatalf("got %v, want 2 pods (excluding kube-system)", names)
	}
	for _, n := range names {
		if n == "kube-stuff" {
			t.Errorf("kube-system pod %q leaked through exclusion", n)
		}
	}
}

func TestInit_RegistersProvider(t *testing.T) {
	if _, ok := core.Lookup("kubernetes"); !ok {
		t.Fatal("kubernetes provider not registered by init()")
	}
}

func TestRawOf_RoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata":   map[string]any{"name": "x"},
	}}
	raw := p.rawOf(u.Object)
	if raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back["kind"] != "ConfigMap" {
		t.Errorf("Raw.kind = %v", back["kind"])
	}
}
