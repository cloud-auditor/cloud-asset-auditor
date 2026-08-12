package server

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The spec is hand-maintained. openapi_test.go already checks the
// documented→handler direction (nothing in the spec is a lie about what
// exists). This file closes the other direction: every route the server
// actually serves must be described.
//
// That gap was real — a handler added without a spec entry passed CI while
// leaving the published contract silently incomplete, which is worse than an
// obviously-missing endpoint because clients generate from the spec.

// apiPatternsForSpec returns the recorded routes that the spec is expected to
// document, normalised to "METHOD /path".
func apiPatternsForSpec(t *testing.T) []string {
	t.Helper()
	s, err := New(Config{AuthMode: "none"})
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, p := range s.patterns {
		// /healthz and /metrics are infrastructure, deliberately outside the
		// /api/v1 namespace and outside the documented API contract.
		if !strings.Contains(p, "/api/v1/") {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		t.Fatal("no /api/v1 routes recorded — did routes() stop using handle/handleFunc?")
	}
	return out
}

func specPaths(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var doc struct {
		Paths map[string]map[string]any `yaml:"paths"`
	}
	if err := yaml.Unmarshal(OpenAPISpec, &doc); err != nil {
		t.Fatalf("openapi.yaml is not valid YAML: %v", err)
	}
	out := map[string]map[string]bool{}
	for path, ops := range doc.Paths {
		methods := map[string]bool{}
		for method := range ops {
			methods[strings.ToUpper(method)] = true
		}
		out[path] = methods
	}
	return out
}

func TestOpenAPI_EveryHandlerIsDocumented(t *testing.T) {
	spec := specPaths(t)

	for _, pattern := range apiPatternsForSpec(t) {
		method, path, ok := strings.Cut(pattern, " ")
		if !ok {
			t.Errorf("route %q has no method prefix; the sync check assumes \"METHOD /path\"", pattern)
			continue
		}
		methods, documented := spec[path]
		if !documented {
			t.Errorf("route %s is served but absent from openapi.yaml — add it (see CLAUDE.md: keep the spec in sync with routes())", pattern)
			continue
		}
		if !methods[method] {
			t.Errorf("path %s is documented but not for %s (documented: %v)", path, method, keysOfBool(methods))
		}
	}
}

// Both verbs of a two-verb endpoint must be documented. GET and POST on
// /topology and /reach do genuinely different things — GET runs a fresh audit,
// POST works from supplied assets — so documenting only one is a real gap, not
// a formality.
func TestOpenAPI_TwoVerbEndpointsDocumentBothVerbs(t *testing.T) {
	spec := specPaths(t)
	for _, path := range []string{"/api/v1/topology", "/api/v1/reach"} {
		methods, ok := spec[path]
		if !ok {
			t.Errorf("%s is not documented at all", path)
			continue
		}
		for _, m := range []string{"GET", "POST"} {
			if !methods[m] {
				t.Errorf("%s is missing its %s operation in openapi.yaml", path, m)
			}
		}
	}
}

// Every $ref in the spec must resolve. A dangling reference makes client
// generators fail on a document that otherwise parses fine, so it survives a
// plain YAML-validity check.
func TestOpenAPI_AllRefsResolve(t *testing.T) {
	var doc map[string]any
	if err := yaml.Unmarshal(OpenAPISpec, &doc); err != nil {
		t.Fatal(err)
	}

	var refs []string
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for k, child := range t {
				if k == "$ref" {
					if s, ok := child.(string); ok {
						refs = append(refs, s)
					}
					continue
				}
				walk(child)
			}
		case []any:
			for _, child := range t {
				walk(child)
			}
		}
	}
	walk(doc)

	if len(refs) == 0 {
		t.Fatal("no $refs found — the walker is broken, not the spec")
	}
	for _, ref := range refs {
		if !strings.HasPrefix(ref, "#/") {
			t.Errorf("external $ref %q — the spec must be self-contained", ref)
			continue
		}
		if !resolveRef(doc, ref) {
			t.Errorf("dangling $ref %q — nothing defines it", ref)
		}
	}
}

// resolveRef walks a "#/a/b/c" JSON pointer through the parsed document.
func resolveRef(doc map[string]any, ref string) bool {
	cur := any(doc)
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		cur, ok = m[part]
		if !ok {
			return false
		}
	}
	return true
}

func keysOfBool(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
