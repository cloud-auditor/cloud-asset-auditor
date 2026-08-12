package server

import (
	"context"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// scopeProvider is a do-nothing provider used to populate the registry, so
// the "fall back to everything registered" branch has something to fall back
// to. The server package imports no providers of its own.
type scopeProvider struct{ name string }

func (p scopeProvider) Name() string                   { return p.name }
func (p scopeProvider) Validate(context.Context) error { return nil }
func (p scopeProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	a := make(chan core.Asset)
	e := make(chan error)
	close(a)
	close(e)
	return a, e
}

func registerScopeProviders(t *testing.T) {
	t.Helper()
	// core.Register panics on a duplicate and has no deregister, so
	// registration is once per test binary rather than per test.
	for _, n := range []string{"scope-alpha", "scope-beta"} {
		if _, ok := core.Lookup(n); ok {
			continue
		}
		name := n
		core.Register(name, func() (core.Provider, error) { return scopeProvider{name}, nil })
	}
}

func names(ps []core.Provider) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.Name()
	}
	return out
}

func TestSelectProviders_ConfigScopeIsTheDefault(t *testing.T) {
	registerScopeProviders(t)
	s := &Server{cfg: Config{Providers: []string{"scope-alpha"}}}

	got, initErrs := s.selectProviders(nil, reqOptions{})
	if len(initErrs) != 0 {
		t.Fatalf("unexpected init errors: %v", initErrs)
	}
	if len(got) != 1 || got[0].Name() != "scope-alpha" {
		t.Fatalf("a request naming no providers must use the configured scope, got %v", names(got))
	}
}

func TestSelectProviders_RequestOverridesConfigScope(t *testing.T) {
	registerScopeProviders(t)
	s := &Server{cfg: Config{Providers: []string{"scope-alpha"}}}

	got, _ := s.selectProviders([]string{"scope-beta"}, reqOptions{})
	if len(got) != 1 || got[0].Name() != "scope-beta" {
		t.Fatalf("an explicit request must win over the configured scope, got %v", names(got))
	}
}

func TestSelectProviders_EmptyConfigStillMeansEverything(t *testing.T) {
	registerScopeProviders(t)
	s := &Server{cfg: Config{}}

	got, _ := s.selectProviders(nil, reqOptions{})
	if len(got) < 2 {
		t.Fatalf("an unscoped server must still run every registered provider, got %v", names(got))
	}
}
