package server_test

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: cluster-a
  cluster: {server: https://127.0.0.1:6443}
- name: cluster-b
  cluster: {server: https://127.0.0.1:6444}
contexts:
- name: ctx-a
  context: {cluster: cluster-a, user: user-a}
- name: ctx-b
  context: {cluster: cluster-b, user: user-b}
current-context: ctx-a
users:
- name: user-a
  user: {token: aaa}
- name: user-b
  user: {token: bbb}
`

// setKubeconfig points the process at a two-context kubeconfig for the test.
func setKubeconfig(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", path)
}

func TestKubeContexts_ListsFromKubeconfig(t *testing.T) {
	setKubeconfig(t)
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/kube/contexts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got struct {
		Contexts []string `json:"contexts"`
		Current  string   `json:"current"`
		Error    string   `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got.Contexts) != 2 || got.Contexts[0] != "ctx-a" || got.Contexts[1] != "ctx-b" {
		t.Errorf("contexts = %v, want [ctx-a ctx-b]", got.Contexts)
	}
	if got.Current != "ctx-a" {
		t.Errorf("current = %q, want ctx-a", got.Current)
	}
	if got.Error != "" {
		t.Errorf("unexpected error field: %q", got.Error)
	}
}

func TestKubeContexts_EmptyWhenNoKubeconfig(t *testing.T) {
	// No kubeconfig and not in-cluster: empty list, 200, non-null contexts.
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "missing"))
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/kube/contexts")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	// The JSON must carry an empty array, never null (the UI maps over it).
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["contexts"]) != "[]" {
		t.Errorf("contexts = %s, want []", raw["contexts"])
	}
}

func TestExport_DropsUnknownKubeContext(t *testing.T) {
	setKubeconfig(t)
	ts := newTestServer(t, server.Config{})

	// providers=none keeps this fast (no real cluster scan) while still
	// exercising validateKubeContexts. The unknown context must be reported
	// via the init-errors header; the known one is accepted silently.
	resp, err := http.Get(ts.URL + "/api/v1/audit/export?format=csv&providers=none&kube_contexts=ctx-a,ghost")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	warn := resp.Header.Get("X-Auditor-Init-Errors")
	if warn == "" {
		t.Fatal("expected X-Auditor-Init-Errors header for the unknown context")
	}
	if !contains(warn, "ghost") {
		t.Errorf("warning = %q, want it to name the dropped context 'ghost'", warn)
	}
	if contains(warn, "ctx-a") {
		t.Errorf("warning = %q should not mention the valid context ctx-a", warn)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
