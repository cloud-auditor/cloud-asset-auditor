package kubecontext

import (
	"os"
	"path/filepath"
	"testing"
)

const twoContextKubeconfig = `apiVersion: v1
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
current-context: ctx-b
users:
- name: user-a
  user:
    token: aaa
- name: user-b
  user:
    token: bbb
`

func writeKubeconfig(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)
}

func TestList_ReturnsSortedContextsAndCurrent(t *testing.T) {
	writeKubeconfig(t, twoContextKubeconfig)

	names, current, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if current != "ctx-b" {
		t.Errorf("current = %q, want ctx-b", current)
	}
	if len(names) != 2 || names[0] != "ctx-a" || names[1] != "ctx-b" {
		t.Errorf("names = %v, want [ctx-a ctx-b] (sorted)", names)
	}
}

func TestList_InClusterReturnsEmpty(t *testing.T) {
	// In a pod there's no kubeconfig to enumerate — a single API server.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	names, current, err := List()
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(names) != 0 || current != "" {
		t.Errorf("in-cluster List() = (%v, %q), want empty", names, current)
	}
}

func TestList_NoKubeconfigIsEmptyNotError(t *testing.T) {
	// Point KUBECONFIG at a path that doesn't exist: clientcmd treats a
	// missing file as an empty config, so we expect no contexts and no error.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "does-not-exist"))

	names, _, err := List()
	if err != nil {
		t.Fatalf("err = %v, want nil for a missing kubeconfig", err)
	}
	if len(names) != 0 {
		t.Errorf("names = %v, want empty", names)
	}
}
