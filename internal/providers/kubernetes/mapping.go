package kubernetes

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// formatType renders the canonical Asset.Type for a Kubernetes object.
//
//	apiVersion = "v1"             kind = "Pod"        → "v1.Pod"
//	apiVersion = "apps/v1"        kind = "Deployment" → "apps/v1.Deployment"
//	apiVersion = "example.com/v1" kind = "Widget"     → "example.com/v1.Widget"
//
// Core resources have apiVersion = "v1" (no group); the period separator
// makes the resulting type stable to split on in downstream tools.
func formatType(apiVersion, kind string) string {
	if apiVersion == "" {
		return kind
	}
	return apiVersion + "." + kind
}

// extractStatus pulls a human-meaningful status string from the
// unstructured object when one is obvious. Pods have status.phase, most
// other resources expose status conditions — for those, we settle for the
// most-recent Ready/Available condition's status. Returns "" when nothing
// useful is available rather than fabricating one.
func extractStatus(u *unstructured.Unstructured) string {
	if phase, ok, _ := unstructured.NestedString(u.Object, "status", "phase"); ok && phase != "" {
		return phase
	}
	conds, ok, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	if !ok {
		return ""
	}
	// Prefer Ready, then Available, then the last condition we see.
	var fallback string
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok {
			continue
		}
		t, _ := m["type"].(string)
		s, _ := m["status"].(string)
		if s == "" {
			continue
		}
		switch t {
		case "Ready":
			return t + "=" + s
		case "Available":
			fallback = t + "=" + s
		default:
			if fallback == "" {
				fallback = t + "=" + s
			}
		}
	}
	return fallback
}

// collapseTags merges the object's labels into the Asset.Tags map and adds
// the namespace as a pseudo-tag so downstream filtering by namespace works
// from CSV/JSON output alone.
func collapseTags(labels map[string]string, namespace string) map[string]string {
	if len(labels) == 0 && namespace == "" {
		return nil
	}
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if namespace != "" {
		out["namespace"] = namespace
	}
	return out
}

// isSubresource reports whether an APIResource name represents a
// subresource (status, scale, etc.). Those have "parent/sub" names; we
// never list them as top-level inventory items.
func isSubresource(name string) bool { return strings.Contains(name, "/") }

// supportsList returns true when the resource's verb list includes "list".
// We're tolerant on case because some aggregated APIs have been observed
// reporting verbs with mixed casing.
func supportsList(verbs []string) bool {
	for _, v := range verbs {
		if strings.EqualFold(v, "list") {
			return true
		}
	}
	return false
}
