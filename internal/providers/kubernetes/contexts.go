package kubernetes

import (
	"os"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/kubecontext"
)

// resolveContexts returns the non-empty list of kubeconfig contexts this audit
// should scan. Precedence:
//
//  1. In-cluster (KUBERNETES_SERVICE_HOST set) → a single "" (the pod's own
//     API server); --kube-contexts is meaningless there.
//  2. Explicit KubeContexts, with the "all" sentinel expanded to every
//     context in the kubeconfig.
//  3. The singular KubeContext (which may be "" → current-context).
//
// The empty string is the well-understood "use kubeconfig current-context /
// in-cluster" marker the single-cluster path already honors.
func (p *Provider) resolveContexts() ([]string, error) {
	if os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		return []string{""}, nil
	}

	raw := p.cfg.KubeContexts
	if len(raw) == 0 {
		return []string{p.cfg.KubeContext}, nil
	}

	if len(raw) == 1 && strings.EqualFold(strings.TrimSpace(raw[0]), allContextsSentinel) {
		names, _, err := kubecontext.List()
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			// Nothing to expand to — fall back to current-context rather than
			// scanning nothing.
			return []string{""}, nil
		}
		return names, nil
	}

	return dedupeContexts(raw), nil
}

// dedupeContexts trims and de-duplicates the requested context names while
// preserving first-seen order, dropping empties (an empty entry in an
// explicit list is almost certainly a stray comma, not "current-context").
func dedupeContexts(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, name := range in {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}
