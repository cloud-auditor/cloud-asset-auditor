package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

// sampleAssetsJSON is a minimal cross-provider chain that the resolvers can
// join: a CF DNS A record pointing at an OCI LB IP, the LB exposing that IP,
// and a CF page rule bound to a zone — yielding one dns edge and one waf edge.
const sampleAssetsJSON = `[
  {"provider":"cloudflare","account_id":"acct-1","type":"cloudflare.zone","id":"zone-1","name":"example.com"},
  {"provider":"cloudflare","account_id":"acct-1","type":"cloudflare.dns_record","id":"rec-1","name":"www.example.com",
   "tags":{"type":"A","content":"203.0.113.7","zone_id":"zone-1","zone_name":"example.com"}},
  {"provider":"oci","account_id":"tenancy-1","type":"oci.load_balancer","id":"lb-1","name":"edge-lb",
   "tags":{"ip_addresses":"203.0.113.7"}},
  {"provider":"cloudflare","account_id":"acct-1","type":"cloudflare.page_rule","id":"pr-1","name":"example.com/*",
   "tags":{"zone_id":"zone-1","zone_name":"example.com"}}
]`

func TestTopologyPost_BareArrayBuildsEdges(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Post(ts.URL+"/api/v1/topology", "application/json", strings.NewReader(sampleAssetsJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200; body: %s", resp.StatusCode, body)
	}

	var out struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []struct {
			Kind       string `json:"kind"`
			Confidence string `json:"confidence"`
		} `json:"edges"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Edges) != 2 {
		t.Fatalf("edges = %d, want 2 (dns + waf)", len(out.Edges))
	}
	kinds := map[string]bool{}
	for _, e := range out.Edges {
		kinds[e.Kind] = true
	}
	if !kinds["dns"] || !kinds["waf"] {
		t.Errorf("edge kinds = %v, want dns and waf", kinds)
	}
	// Orphans dropped by default: all four assets participate in edges
	// except none — zone, dns record, lb, page rule all touch an edge.
	if len(out.Nodes) != 4 {
		t.Errorf("nodes = %d, want 4", len(out.Nodes))
	}
}

func TestTopologyPost_EnvelopeAndIncludeOrphans(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	envelope := `{"assets": [
	  {"provider":"oci","type":"oci.instance","id":"inst-1","name":"lonely-vm"}
	]}`

	resp, err := http.Post(ts.URL+"/api/v1/topology?include-orphans=true", "application/json", strings.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Nodes []map[string]any `json:"nodes"`
		Edges []any            `json:"edges"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 1 || len(out.Edges) != 0 {
		t.Errorf("nodes/edges = %d/%d, want 1/0", len(out.Nodes), len(out.Edges))
	}

	// Same body without include-orphans: the lone node is dropped.
	resp2, err := http.Post(ts.URL+"/api/v1/topology", "application/json", strings.NewReader(envelope))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		Nodes []map[string]any `json:"nodes"`
	}
	if err := json.NewDecoder(resp2.Body).Decode(&out2); err != nil {
		t.Fatal(err)
	}
	if len(out2.Nodes) != 0 {
		t.Errorf("nodes = %d, want 0 (orphans dropped by default)", len(out2.Nodes))
	}
}

func TestTopologyPost_DotFormatDownload(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Post(ts.URL+"/api/v1/topology?format=dot", "application/json", strings.NewReader(sampleAssetsJSON))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Type"); got != "text/vnd.graphviz" {
		t.Errorf("Content-Type = %q, want text/vnd.graphviz", got)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "topology.dot") {
		t.Errorf("Content-Disposition = %q, want topology.dot filename", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "digraph") {
		t.Errorf("body does not look like DOT output: %.120s", body)
	}
}

func TestTopologyPost_BadBodyIs400(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	for _, body := range []string{"", "not json", `{"assets": "nope"}`} {
		resp, err := http.Post(ts.URL+"/api/v1/topology", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q: status = %d, want 400", body, resp.StatusCode)
		}
	}
}

func TestTopologyPost_RequiresAuthLikeOtherAPIRoutes(t *testing.T) {
	ts := newTestServer(t, server.Config{AuthMode: "token", APIToken: "sekrit"})

	resp, err := http.Post(ts.URL+"/api/v1/topology", "application/json", strings.NewReader(sampleAssetsJSON))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", resp.StatusCode)
	}

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/topology", strings.NewReader(sampleAssetsJSON))
	req.Header.Set("Authorization", "Bearer sekrit")
	req.Header.Set("Content-Type", "application/json")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", resp2.StatusCode)
	}
}
