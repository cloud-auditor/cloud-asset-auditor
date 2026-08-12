// Package kubernetes is the Phase 4 provider. It uses the dynamic client +
// discovery (not typed clients) so CRDs are inventoried without any
// per-resource code. See init-plan.md §3 Phase 4.
package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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
	KubeContext  string
	KubeContexts []string // multi-cluster scan; "all" = every kubeconfig context. Wins over KubeContext.

	KubeNamespace         string   // empty means all namespaces (minus excluded)
	KubeExcludeNamespaces []string // ignored when KubeNamespace is set
	ExcludeHelmSecrets    bool     // skip Helm v3 release-state Secrets
	ExcludeEvents         bool     // skip Event objects (core v1 + events.k8s.io) at discovery
	MaxConcurrency        int
	IncludeRaw            bool
}

// allContextsSentinel passed via --kube-contexts (or the API) expands to
// every context in the resolved kubeconfig.
const allContextsSentinel = "all"

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

// SetKubeContexts sets the multi-cluster context list. A nil/empty slice is
// ignored so "user didn't pass --kube-contexts" can't blank out a value set
// via the singular SetKubeContext.
func (p *Provider) SetKubeContexts(names []string) {
	if len(names) > 0 {
		p.cfg.KubeContexts = names
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

func (p *Provider) SetKubeExcludeEvents(b bool) { p.cfg.ExcludeEvents = b }

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

// assetID returns a stable identity for an object.
//
// The UID is right when there is one, but not every listable resource has one:
// objects the API server *computes* rather than stores carry an empty UID.
// Against a real cluster that is metrics.k8s.io PodMetrics and NodeMetrics
// plus ComponentStatus — 65 of 1,568 assets on the cluster this was found on,
// every one of them landing on the same empty ID.
//
// That is not cosmetic. Asset identity is (provider, id): `auditor diff` keys
// drift on it, the topology index buckets byID, and the audit cache stores by
// it — so every UID-less object in a cluster collapses into a single slot and
// the last one silently wins.
//
// The fallback is the object's own coordinates, which is exactly what makes it
// unique within a cluster and stable across runs: apiVersion, kind, namespace,
// name. It is prefixed so a synthesized id can never be mistaken for, or
// collide with, a real UID.
func (p *Provider) assetID(u *unstructured.Unstructured) string {
	if uid := string(u.GetUID()); uid != "" {
		return uid
	}
	parts := []string{"k8s", u.GetAPIVersion(), u.GetKind()}
	if ns := u.GetNamespace(); ns != "" {
		parts = append(parts, ns)
	}
	return strings.Join(append(parts, u.GetName()), "/")
}

// unstructuredToAsset is the universal mapper — works for every Kubernetes
// resource (built-in or CRD) because every object has the same metadata
// shape under the hood.
func (p *Provider) unstructuredToAsset(u *unstructured.Unstructured) core.Asset {
	a := core.Asset{
		Provider:  providerName,
		AccountID: p.clusterID,
		Type:      formatType(u.GetAPIVersion(), u.GetKind()),
		ID:        p.assetID(u),
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
