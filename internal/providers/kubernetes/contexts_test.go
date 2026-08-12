package kubernetes

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestDedupeContexts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"trims and de-dups, preserves order", []string{" a ", "b", "a", "b "}, []string{"a", "b"}},
		{"drops empties (stray commas)", []string{"a", "", "  ", "b"}, []string{"a", "b"}},
		{"all empty falls back to current-context", []string{"", "  "}, []string{""}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := dedupeContexts(c.in); !reflect.DeepEqual(got, c.want) {
				t.Errorf("dedupeContexts(%v) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestResolveContexts_SingularAndDefault(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	// No contexts configured at all → single empty (current-context).
	if got, _ := (&Provider{}).resolveContexts(); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("default resolveContexts = %v, want [\"\"]", got)
	}

	// Singular KubeContext is honored when KubeContexts is empty.
	p := &Provider{cfg: Config{KubeContext: "prod"}}
	if got, _ := p.resolveContexts(); !reflect.DeepEqual(got, []string{"prod"}) {
		t.Errorf("singular resolveContexts = %v, want [prod]", got)
	}

	// Plural wins over singular.
	p = &Provider{cfg: Config{KubeContext: "prod", KubeContexts: []string{"a", "b"}}}
	if got, _ := p.resolveContexts(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("plural resolveContexts = %v, want [a b]", got)
	}
}

func TestResolveContexts_InClusterCollapsesToSingle(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	// Even with an explicit multi list, in-cluster collapses to the pod's
	// own API server — there's nothing else to reach.
	p := &Provider{cfg: Config{KubeContexts: []string{"a", "b", "all"}}}
	if got, _ := p.resolveContexts(); !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("in-cluster resolveContexts = %v, want [\"\"]", got)
	}
}

func TestResolveContexts_AllSentinelExpands(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)

	p := &Provider{cfg: Config{KubeContexts: []string{"all"}}}
	got, err := p.resolveContexts()
	if err != nil {
		t.Fatal(err)
	}
	// testKubeconfig (auth_test.go) defines ctx-a and ctx-b, sorted.
	if !reflect.DeepEqual(got, []string{"ctx-a", "ctx-b"}) {
		t.Errorf("\"all\" resolveContexts = %v, want [ctx-a ctx-b]", got)
	}
}

func TestResolveContexts_AllSentinelEmptyKubeconfigFallsBack(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))

	p := &Provider{cfg: Config{KubeContexts: []string{"all"}}}
	got, err := p.resolveContexts()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{""}) {
		t.Errorf("\"all\" with no kubeconfig = %v, want [\"\"] fallback", got)
	}
}

func TestSetKubeContexts(t *testing.T) {
	p := New(Config{})
	p.SetKubeContexts([]string{"a", "b"})
	if !reflect.DeepEqual(p.cfg.KubeContexts, []string{"a", "b"}) {
		t.Errorf("KubeContexts = %v, want [a b]", p.cfg.KubeContexts)
	}
	// nil/empty must not clobber a previously-set value.
	p.SetKubeContexts(nil)
	if !reflect.DeepEqual(p.cfg.KubeContexts, []string{"a", "b"}) {
		t.Errorf("nil clobbered KubeContexts: %v", p.cfg.KubeContexts)
	}
}
