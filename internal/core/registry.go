package core

import (
	"fmt"
	"sort"
	"sync"
)

// Factory builds a Provider once configuration is available. Providers
// register a Factory (not an instance) so package init stays cheap and
// credential checks defer until the CLI has loaded config.
type Factory func() (Provider, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register installs a provider Factory under name. Re-registering the same
// name panics — that's a programmer error (duplicate init), not a user error.
func Register(name string, factory Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic(fmt.Sprintf("core: provider %q already registered", name))
	}
	registry[name] = factory
}

// Lookup returns the Factory for name, or false if not registered.
func Lookup(name string) (Factory, bool) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[name]
	return f, ok
}

// Registered returns all registered provider names in sorted order.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
