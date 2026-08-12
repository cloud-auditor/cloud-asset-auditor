package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// reachBody is the exposure chain the handler tests post: a public DNS record
// resolving to an OCI load balancer that fronts a Kubernetes Service.
func reachBody() string {
	assets := []core.Asset{
		{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "api.example.com",
			Tags: map[string]string{"type": "A", "content": "203.0.113.10"}},
		{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "prod-lb",
			Tags: map[string]string{"ip_addresses": "203.0.113.10"}},
		// The Service carries a published load-balancer address as well as its
		// selector. Both matter: the address is what lets lbToGateway join it
		// to the OCI load balancer (without it the graph is two disconnected
		// components and no route runs end to end), and the selector is what
		// serviceToWorkload uses to reach the pod.
		{Provider: "kubernetes", AccountID: "c1", Type: "v1.Service", ID: "svc1", Name: "api",
			Tags: map[string]string{"namespace": "shop"},
			Raw: json.RawMessage(`{"spec":{"selector":{"app":"api"}},` +
				`"status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.10"}]}}}`)},
		{Provider: "kubernetes", AccountID: "c1", Type: "v1.Pod", ID: "pod1", Name: "api-abc",
			Tags: map[string]string{"namespace": "shop", "app": "api"}},
	}
	b, err := json.Marshal(map[string]any{"assets": assets})
	if err != nil {
		panic(err)
	}
	return string(b)
}

func postReach(t *testing.T, srv *httptest.Server, query string) (*http.Response, []byte) {
	t.Helper()
	res, err := http.Post(srv.URL+"/api/v1/reach"+query, "application/json", strings.NewReader(reachBody()))
	if err != nil {
		t.Fatalf("POST /api/v1/reach%s: %v", query, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read reach response: %v", err)
	}
	return res, body
}

func decodeReach(t *testing.T, body []byte) topology.ReachResult {
	t.Helper()
	var got topology.ReachResult
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode reach response: %v\nbody: %s", err, body)
	}
	return got
}

func TestReach_ExposedFindsTheChain(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, body := postReach(t, srv, "?exposed=true")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d: %s", res.StatusCode, body)
	}
	got := decodeReach(t, body)

	if !strings.Contains(got.Question, "internet") {
		t.Errorf("Question = %q", got.Question)
	}
	if len(got.Paths) == 0 {
		t.Fatalf("expected exposure paths; got %s", body)
	}
	// Every route must start at the outermost public surface, not mid-chain.
	for _, p := range got.Paths {
		if p.Nodes[0].Type != "cloudflare.dns_record" {
			t.Errorf("path starts at %s (%s), want the DNS record", p.Nodes[0].ID, p.Nodes[0].Type)
		}
	}
	if len(got.Sources) == 0 {
		t.Error("response should carry the entry points it started from")
	}
}

func TestReach_QueryModes(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for _, tc := range []struct {
		name, query, wantQuestion string
	}{
		{"downstream", "?from=api.example.com", "What can"},
		{"upstream", "?to=api-abc", "What can reach"},
		{"trace", "?from=api.example.com&to=api-abc", "How can"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, body := postReach(t, srv, tc.query)
			if res.StatusCode != http.StatusOK {
				t.Fatalf("status = %d: %s", res.StatusCode, body)
			}
			got := decodeReach(t, body)
			if !strings.Contains(got.Question, tc.wantQuestion) {
				t.Errorf("Question = %q, want it to mention %q", got.Question, tc.wantQuestion)
			}
			if len(got.Paths) == 0 {
				t.Errorf("no paths for %s", tc.query)
			}
		})
	}
}

// An unconstrained reachability query has no meaningful answer, so it must be
// rejected rather than silently returning the whole graph or nothing.
func TestReach_NoSelectorIs400(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, body := postReach(t, srv, "")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", res.StatusCode)
	}
	for _, want := range []string{"from", "to", "exposed"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("error should name the %q parameter: %s", want, body)
		}
	}
}

// A selector that matches nothing is a 400, not an empty 200 — an empty result
// reads as "nothing can reach it", the opposite of "your selector was wrong".
func TestReach_UnmatchedSelectorIs400(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, body := postReach(t, srv, "?to=definitely-not-here")
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body: %s", res.StatusCode, body)
	}
	if !strings.Contains(string(body), "matched no assets") {
		t.Errorf("body should explain the selector matched nothing: %s", body)
	}
}

func TestReach_OptionsAreHonoured(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	t.Run("max_hops bounds path length", func(t *testing.T) {
		_, body := postReach(t, srv, "?from=api.example.com&max_hops=1")
		for _, p := range decodeReach(t, body).Paths {
			if len(p.Edges) > 1 {
				t.Errorf("path of %d hops exceeds max_hops=1", len(p.Edges))
			}
		}
	})

	t.Run("max_paths caps and reports truncation", func(t *testing.T) {
		_, body := postReach(t, srv, "?from=api.example.com&max_paths=1")
		got := decodeReach(t, body)
		if len(got.Paths) != 1 {
			t.Errorf("got %d paths, want 1", len(got.Paths))
		}
		if !got.Truncated {
			t.Error("hitting max_paths must set truncated — a capped security result read as complete is the dangerous case")
		}
	})

	t.Run("kinds restricts traversal", func(t *testing.T) {
		_, body := postReach(t, srv, "?from=api.example.com&kinds=dns")
		for _, p := range decodeReach(t, body).Paths {
			for _, e := range p.Edges {
				if e.Kind != "dns" {
					t.Errorf("edge kind %q traversed despite kinds=dns", e.Kind)
				}
			}
		}
	})

	t.Run("unparseable numbers fall back to the default", func(t *testing.T) {
		res, body := postReach(t, srv, "?from=api.example.com&max_hops=abc")
		if res.StatusCode != http.StatusOK {
			t.Errorf("a junk max_hops should fall back to the default, not 400: %d %s", res.StatusCode, body)
		}
	})
}

// Non-JSON formats come back as attachments, so dragging the URL into a file
// manager saves something openable.
func TestReach_RenderedFormatsAreDownloads(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for _, format := range []string{"dot", "mermaid", "d2", "excalidraw"} {
		res, body := postReach(t, srv, "?exposed=true&format="+format)
		if res.StatusCode != http.StatusOK {
			t.Errorf("format=%s → %d", format, res.StatusCode)
			continue
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Errorf("format=%s Content-Disposition = %q, want an attachment", format, cd)
		}
		if len(body) == 0 {
			t.Errorf("format=%s produced an empty body", format)
		}
	}
}

func TestReach_RejectsUndecodableBody(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/v1/reach?exposed=true", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

// Reach sits under /api/v1, so it must be behind the auth gate — unlike
// /healthz, /metrics, and the spec, which are deliberately exempt.
func TestReach_RequiresAuthWhenConfigured(t *testing.T) {
	s, err := New(Config{AuthMode: "token", APIToken: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	res, err := http.Post(srv.URL+"/api/v1/reach?exposed=true", "application/json", strings.NewReader(reachBody()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauthenticated POST /api/v1/reach → %d, want 401", res.StatusCode)
	}
}

// buildReachResult is the shared question-dispatch; exercised directly for the
// branches the HTTP tests can't easily reach.
func TestBuildReachResult_TargetsAreReported(t *testing.T) {
	dns := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "a.example.com"}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "lb"}
	topo := &topology.Topology{
		Nodes: []core.Asset{dns, lb},
		Edges: []core.Edge{{From: dns.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceHeuristic}},
	}

	got, err := buildReachResult(topo, "a.example.com", "lb", false, topology.ReachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 1 || got.Sources[0].ID != "d1" {
		t.Errorf("Sources = %+v", got.Sources)
	}
	if len(got.Targets) != 1 || got.Targets[0].ID != "lb1" {
		t.Errorf("Targets = %+v", got.Targets)
	}
}

func TestAtoiOr(t *testing.T) {
	for _, tc := range []struct {
		in       string
		fallback int
		want     int
	}{
		{"", 6, 6},
		{"12", 6, 12},
		// Junk falls back rather than erroring: a mistyped query parameter
		// should not fail a security query outright.
		{"abc", 6, 6},
		{"-3", 6, -3},
	} {
		if got := atoiOr(tc.in, tc.fallback); got != tc.want {
			t.Errorf("atoiOr(%q, %d) = %d, want %d", tc.in, tc.fallback, got, tc.want)
		}
	}
}
