// Package kubernetes is the Phase 4 provider. It uses the dynamic client +
// discovery (not typed clients) so CRDs are inventoried without any
// per-resource code. See init-plan.md §3 Phase 4.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
	providerName          = "kubernetes"
	defaultMaxConcurrency = 5
)

// Default namespaces filtered out unless the user explicitly opts back in
// by passing --kube-exclude-namespaces with a different set.
var defaultExcludedNamespaces = []string{"kube-system", "kube-public", "kube-node-lease"}

// Config drives provider construction. Everything is optional; sensible
// defaults are applied in New.
type Config struct {
	KubeContext           string
	KubeNamespace         string   // empty means all namespaces (minus excluded)
	KubeExcludeNamespaces []string // ignored when KubeNamespace is set
	ExcludeHelmSecrets    bool     // skip Helm v3 release-state Secrets
	MaxConcurrency        int
	IncludeRaw            bool
}

// Provider implements core.Provider for Kubernetes. Like the OCI provider,
// auth and clients resolve lazily so factory construction is cheap.
type Provider struct {
	cfg Config

	clientOnce sync.Once
	restCfg    *rest.Config
	discovery  discovery.DiscoveryInterface
	dynamic    dynamic.Interface
	clusterID  string // populated alongside the clients
	clientErr  error
}

var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
	_ core.KubeConfigurable        = (*Provider)(nil)
)

func init() {
	core.Register(providerName, func() (core.Provider, error) {
		return New(Config{}), nil
	})
}

// New constructs a Provider with defaults. Auth + client construction is
// deferred to first use so the factory never errors at registration time.
func New(cfg Config) *Provider {
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = defaultMaxConcurrency
	}
	if cfg.KubeExcludeNamespaces == nil {
		cfg.KubeExcludeNamespaces = defaultExcludedNamespaces
	}
	return &Provider{cfg: cfg}
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SetMaxConcurrency(n int) {
	if n > 0 {
		p.cfg.MaxConcurrency = n
	}
}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

func (p *Provider) SetKubeContext(s string) {
	if s != "" {
		p.cfg.KubeContext = s
	}
}

func (p *Provider) SetKubeNamespace(s string) { p.cfg.KubeNamespace = s }

func (p *Provider) SetKubeExcludeNamespaces(ns []string) {
	// Only override defaults when the caller actually passed a value.
	// A nil slice from "user didn't touch the flag" mustn't blank out the
	// default exclusion list.
	if ns != nil {
		p.cfg.KubeExcludeNamespaces = ns
	}
}

func (p *Provider) SetKubeExcludeHelmSecrets(b bool) { p.cfg.ExcludeHelmSecrets = b }

// ensureClients resolves the REST config, builds the discovery + dynamic
// clients, and records a cluster identifier — all exactly once.
func (p *Provider) ensureClients() error {
	p.clientOnce.Do(func() {
		restCfg, clusterID, err := loadRESTConfig(p.cfg.KubeContext)
		if err != nil {
			p.clientErr = fmt.Errorf("load kubeconfig: %w", err)
			return
		}
		p.restCfg = restCfg
		p.clusterID = clusterID

		disc, err := discovery.NewDiscoveryClientForConfig(restCfg)
		if err != nil {
			p.clientErr = fmt.Errorf("discovery client: %w", err)
			return
		}
		p.discovery = disc

		dyn, err := dynamic.NewForConfig(restCfg)
		if err != nil {
			p.clientErr = fmt.Errorf("dynamic client: %w", err)
			return
		}
		p.dynamic = dyn
	})
	return p.clientErr
}

// Validate hits ServerVersion — one cheap, unauthenticated-friendly call
// that both proves the cluster is reachable and confirms auth.
func (p *Provider) Validate(ctx context.Context) error {
	if err := p.ensureClients(); err != nil {
		return fmt.Errorf("kubernetes: %w", err)
	}
	if _, err := p.discovery.ServerVersion(); err != nil {
		return fmt.Errorf("kubernetes: server version: %w", err)
	}
	_ = ctx // ServerVersion is context-less in client-go
	return nil
}

func (p *Provider) rawOf(v any) json.RawMessage {
	if !p.cfg.IncludeRaw {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- a:
		return true
	}
}

// unstructuredToAsset is the universal mapper — works for every Kubernetes
// resource (built-in or CRD) because every object has the same metadata
// shape under the hood.
func (p *Provider) unstructuredToAsset(u *unstructured.Unstructured) core.Asset {
	a := core.Asset{
		Provider:  providerName,
		AccountID: p.clusterID,
		Type:      formatType(u.GetAPIVersion(), u.GetKind()),
		ID:        string(u.GetUID()),
		Name:      u.GetName(),
		Status:    extractStatus(u),
		Tags:      collapseTags(u.GetLabels(), u.GetNamespace()),
		Raw:       p.rawOf(u.Object),
	}
	if created := u.GetCreationTimestamp(); !created.IsZero() {
		t := created.Time
		a.CreatedAt = &t
	}
	return a
}
