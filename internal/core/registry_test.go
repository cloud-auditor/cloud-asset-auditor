package core

import (
	"context"
	"testing"
)

type fakeProvider struct{ name string }

func (f *fakeProvider) Name() string                       { return f.name }
func (f *fakeProvider) Validate(_ context.Context) error   { return nil }
func (f *fakeProvider) Collect(_ context.Context) (<-chan Asset, <-chan error) {
	a := make(chan Asset)
	e := make(chan error)
	close(a)
	close(e)
	return a, e
}

// resetRegistry swaps in an empty registry for the duration of the test and
// restores the previous one on cleanup, so tests don't bleed into each other
// or into the binary's global state.
func resetRegistry(t *testing.T) {
	t.Helper()
	registryMu.Lock()
	prev := registry
	registry = map[string]Factory{}
	registryMu.Unlock()
	t.Cleanup(func() {
		registryMu.Lock()
		registry = prev
		registryMu.Unlock()
	})
}

func TestRegistry_RegisterLookupRoundTrip(t *testing.T) {
	resetRegistry(t)

	Register("fake", func() (Provider, error) { return &fakeProvider{name: "fake"}, nil })

	factory, ok := Lookup("fake")
	if !ok {
		t.Fatal("expected fake to be registered")
	}
	p, err := factory()
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Name(); got != "fake" {
		t.Errorf("Name() = %q, want %q", got, "fake")
	}
}

func TestRegistry_RegisteredIsSorted(t *testing.T) {
	resetRegistry(t)

	Register("oci", func() (Provider, error) { return &fakeProvider{name: "oci"}, nil })
	Register("cloudflare", func() (Provider, error) { return &fakeProvider{name: "cloudflare"}, nil })
	Register("kubernetes", func() (Provider, error) { return &fakeProvider{name: "kubernetes"}, nil })

	got := Registered()
	want := []string{"cloudflare", "kubernetes", "oci"}
	if len(got) != len(want) {
		t.Fatalf("Registered() len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("Registered()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestRegistry_DuplicateRegisterPanics(t *testing.T) {
	resetRegistry(t)

	Register("dup", func() (Provider, error) { return &fakeProvider{name: "dup"}, nil })
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register, got none")
		}
	}()
	Register("dup", func() (Provider, error) { return &fakeProvider{name: "dup"}, nil })
}

func TestRegistry_LookupMissing(t *testing.T) {
	resetRegistry(t)
	if _, ok := Lookup("nope"); ok {
		t.Error("Lookup of unregistered name returned ok=true")
	}
}
