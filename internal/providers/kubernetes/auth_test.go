package kubernetes

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: cluster-a
  cluster:
    server: https://127.0.0.1:6443
- name: cluster-b
  cluster:
    server: https://127.0.0.1:6444
contexts:
- name: ctx-a
  context:
    cluster: cluster-a
    user: user-a
- name: ctx-b
  context:
    cluster: cluster-b
    user: user-b
current-context: ctx-a
users:
- name: user-a
  user:
    token: aaa
- name: user-b
  user:
    token: bbb
`

// writeKubeconfig drops a two-context kubeconfig into a temp dir and points
// KUBECONFIG at it, with the in-cluster env neutralised.
func writeKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)
}

func TestLoadRESTConfig_FromKubeconfig(t *testing.T) {
	writeKubeconfig(t)

	cfg, clusterID, err := loadRESTConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want current-context cluster-a's server", cfg.Host)
	}
	if clusterID != "cluster-a" {
		t.Errorf("clusterID = %q, want cluster-a", clusterID)
	}
}

func TestLoadRESTConfig_ContextOverride(t *testing.T) {
	writeKubeconfig(t)

	cfg, clusterID, err := loadRESTConfig("ctx-b")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Host != "https://127.0.0.1:6444" {
		t.Errorf("Host = %q, want ctx-b cluster's server", cfg.Host)
	}
	if clusterID != "cluster-b" {
		t.Errorf("clusterID = %q, want cluster-b", clusterID)
	}
}

func TestLoadRESTConfig_InvalidKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("{{{ this is not yaml"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)

	_, _, err := loadRESTConfig("")
	if err == nil {
		t.Fatal("expected error for unparseable kubeconfig")
	}
	if !strings.Contains(err.Error(), "client config") {
		t.Errorf("error = %v, want wrapped with 'client config'", err)
	}
}

func TestLoadRESTConfig_InClusterError(t *testing.T) {
	// KUBERNETES_SERVICE_HOST set selects the in-cluster branch; with no
	// service port (and no service-account token files on a dev box) the
	// rest.InClusterConfig call must fail, and the error must say so.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")

	_, _, err := loadRESTConfig("")
	if err == nil {
		t.Fatal("expected error outside a real pod")
	}
	if !strings.Contains(err.Error(), "in-cluster config") {
		t.Errorf("error = %v, want wrapped with 'in-cluster config'", err)
	}
}

// stubClientConfig lets us drive clusterIDFrom's RawConfig-error fallbacks.
type stubClientConfig struct {
	raw clientcmdapi.Config
	err error
}

func (s stubClientConfig) RawConfig() (clientcmdapi.Config, error) { return s.raw, s.err }
func (s stubClientConfig) ClientConfig() (*rest.Config, error)     { return nil, nil }
func (s stubClientConfig) Namespace() (string, bool, error)        { return "", false, nil }
func (s stubClientConfig) ConfigAccess() clientcmd.ConfigAccess    { return nil }

func TestClusterIDFrom(t *testing.T) {
	withContexts := clientcmdapi.Config{
		CurrentContext: "prod",
		Contexts: map[string]*clientcmdapi.Context{
			"prod":    {Cluster: "prod-cluster"},
			"staging": {Cluster: "staging-cluster"},
			"bare":    {Cluster: ""}, // context present but cluster unset
		},
	}

	cases := []struct {
		name     string
		cc       clientcmd.ClientConfig
		override string
		want     string
	}{
		{"raw config error with override", stubClientConfig{err: errors.New("boom")}, "ctx-x", "ctx-x"},
		{"raw config error without override", stubClientConfig{err: errors.New("boom")}, "", "unknown-cluster"},
		{"current context resolves cluster", stubClientConfig{raw: withContexts}, "", "prod-cluster"},
		{"override resolves its cluster", stubClientConfig{raw: withContexts}, "staging", "staging-cluster"},
		{"override not in contexts falls back to name", stubClientConfig{raw: withContexts}, "ghost", "ghost"},
		{"context without cluster falls back to name", stubClientConfig{raw: withContexts}, "bare", "bare"},
		{"empty config", stubClientConfig{}, "", "unknown-cluster"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clusterIDFrom(c.cc, c.override); got != c.want {
				t.Errorf("clusterIDFrom = %q, want %q", got, c.want)
			}
		})
	}
}

func TestEnsureClients_FromKubeconfig(t *testing.T) {
	writeKubeconfig(t)

	p := New(Config{})
	if err := p.ensureClients(); err != nil {
		t.Fatal(err)
	}
	if p.clusterID != "cluster-a" {
		t.Errorf("clusterID = %q, want cluster-a", p.clusterID)
	}
	if p.discovery == nil || p.dynamic == nil {
		t.Error("discovery/dynamic clients should be constructed")
	}
	// Memoized: a second call must return the same nil without rework.
	if err := p.ensureClients(); err != nil {
		t.Errorf("second ensureClients = %v", err)
	}
}

func TestEnsureClients_Error_MemoizedAndSurfacedByValidate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("{{{ nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)

	p := New(Config{})
	err := p.ensureClients()
	if err == nil {
		t.Fatal("expected error for broken kubeconfig")
	}
	if !strings.Contains(err.Error(), "load kubeconfig") {
		t.Errorf("error = %v, want wrapped with 'load kubeconfig'", err)
	}
	// The once must memoize the failure too.
	if err2 := p.ensureClients(); !errors.Is(err2, err) && err2.Error() != err.Error() {
		t.Errorf("second call error = %v, want memoized %v", err2, err)
	}
	// Validate reports the same failure with the provider prefix.
	if verr := p.Validate(t.Context()); verr == nil || !strings.Contains(verr.Error(), "kubernetes:") {
		t.Errorf("Validate = %v, want kubernetes-prefixed auth error", verr)
	}
}
