package server_test

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

func TestOpenAPI_ServedAtVersionedPath(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/yaml" {
		t.Errorf("Content-Type = %q, want application/yaml", ct)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) < 100 {
		t.Fatalf("body too short, looks empty: %q", body)
	}
}

func TestOpenAPI_IsValidYAMLAndOpenAPI3(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var doc struct {
		OpenAPI string         `yaml:"openapi"`
		Info    map[string]any `yaml:"info"`
		Paths   map[string]any `yaml:"paths"`
		Comps   map[string]any `yaml:"components"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("served bytes are not valid YAML: %v", err)
	}
	if !strings.HasPrefix(doc.OpenAPI, "3.") {
		t.Errorf("openapi version = %q, want 3.x", doc.OpenAPI)
	}
	if len(doc.Paths) == 0 {
		t.Errorf("paths section is empty")
	}
	if doc.Info["title"] == nil {
		t.Errorf("info.title is missing")
	}
}

func TestOpenAPI_AvailableWithoutAuth(t *testing.T) {
	// Spec must be reachable even when --auth=basic is enabled —
	// client generators don't carry credentials and the spec leaks
	// no secrets.
	ts := newTestServer(t, server.Config{
		AuthMode: "basic", BasicUser: "u", BasicPass: "p",
	})

	resp, err := http.Get(ts.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("openapi.yaml status under auth=basic = %d, want 200", resp.StatusCode)
	}
}

// TestOpenAPI_EveryDocumentedPathHasAHandler is the contract test: every
// path in the spec must be wired up in routes(). Catches the easy mistake
// of documenting a new endpoint and forgetting to register the handler
// (or vice versa).
func TestOpenAPI_EveryDocumentedPathHasAHandler(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}

	for path, ops := range doc.Paths {
		// The spec uses OpenAPI path templates ({id}); strip the
		// braces for the GET probe.
		probe := regexp.MustCompile(`\{[^}]+\}`).ReplaceAllString(path, "test")

		// Hit each documented operation. We only need GET probes —
		// every endpoint in this API is read-only.
		if _, ok := ops["get"]; !ok {
			continue
		}
		resp, err := http.Get(ts.URL + probe)
		if err != nil {
			t.Errorf("documented path %q is unreachable: %v", path, err)
			continue
		}
		resp.Body.Close()
		// 404 = no handler registered. Anything else (200, 400, 401,
		// 500 from an audit failure) is fine — the route exists.
		if resp.StatusCode == http.StatusNotFound {
			t.Errorf("documented path %q returned 404 — handler missing in routes()", path)
		}
	}
}
