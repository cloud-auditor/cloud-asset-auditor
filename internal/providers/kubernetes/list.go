package kubernetes

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// listResource enumerates one (GVR, namespace) target via the dynamic
// client. Returns nil on Forbidden so the SA's missing permissions don't
// abort the whole audit (init-plan.md §3 Phase 4: "warn, don't fail").
func (p *Provider) listResource(ctx context.Context, t resourceTarget, out chan<- core.Asset) error {
	// NamespaceableResourceInterface embeds ResourceInterface, so the
	// same `List` method works for both cluster-wide and single-namespace
	// scopes — just pick the right view.
	var ri dynamic.ResourceInterface = p.dynamic.Resource(t.GVR)
	if t.Namespaced && p.cfg.KubeNamespace != "" {
		ri = p.dynamic.Resource(t.GVR).Namespace(p.cfg.KubeNamespace)
	}

	excluded := p.excludedNamespaceSet()

	var continueToken string
	for {
		resp, err := ri.List(ctx, metav1.ListOptions{
			Limit:    500,
			Continue: continueToken,
		})
		if err != nil {
			if apierrors.IsForbidden(err) || apierrors.IsMethodNotSupported(err) {
				return nil
			}
			return fmt.Errorf("list %s: %w", t.GVR.String(), err)
		}
		for i := range resp.Items {
			item := &resp.Items[i]
			// Honor --kube-exclude-namespaces only when we're not scoped
			// to a single namespace (already filtered by the API server).
			if t.Namespaced && p.cfg.KubeNamespace == "" {
				if _, drop := excluded[item.GetNamespace()]; drop {
					continue
				}
			}
			if p.cfg.ExcludeHelmSecrets && isHelmReleaseSecret(item) {
				continue
			}
			if !sendAsset(ctx, out, p.unstructuredToAsset(item)) {
				return nil
			}
		}
		if resp.GetContinue() == "" {
			return nil
		}
		continueToken = resp.GetContinue()
	}
}

func (p *Provider) excludedNamespaceSet() map[string]struct{} {
	out := make(map[string]struct{}, len(p.cfg.KubeExcludeNamespaces))
	for _, ns := range p.cfg.KubeExcludeNamespaces {
		out[ns] = struct{}{}
	}
	return out
}
