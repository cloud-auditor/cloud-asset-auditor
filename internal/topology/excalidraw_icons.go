package topology

import (
	"encoding/base64"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Embedded service icons for the Excalidraw export. Each is a small,
// original line glyph (24×24 viewBox, stroke-based) drawn here rather than
// vendored from any brand icon pack — keeping the binary self-contained and
// free of trademark/licensing concerns (same spirit as the hand-rolled web
// UI; see CLAUDE.md "don't vendor third-party JS").
//
// Icons are categorised by asset Type, not provider, so a database reads as
// a database whether it's OCI or Cloudflare D1. The provider identity is
// carried separately by the box tint + accent stroke in excalidraw.go.
// Each glyph bakes in its own colour, so a given iconKey maps to exactly one
// file entry (deduplicated by content via a stable fileId).

// icon is a categorised glyph: the SVG path body plus the stroke colour.
type icon struct {
	key   string // stable identifier, also seeds the Excalidraw fileId
	color string // stroke colour, baked into the data URL
	body  string // inner SVG (paths/shapes); wrapped by svgDocument
}

// iconSet is the catalogue, keyed by iconKey. Colours are chosen so related
// services read as a family (storage teal, data orange, edge/security green).
var iconSet = map[string]icon{
	"dns": {"dns", "#2563eb",
		`<circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3c2.6 2.7 2.6 15.3 0 18M12 3c-2.6 2.7-2.6 15.3 0 18"/>`},
	"waf": {"waf", "#16a34a",
		`<path d="M12 3l7 3v5c0 4.6-3 7.6-7 9-4-1.4-7-4.4-7-9V6z"/><path d="M9 12l2 2 4-4"/>`},
	"tunnel": {"tunnel", "#9333ea",
		`<path d="M3 20V12a9 9 0 0 1 18 0v8"/><path d="M8 20v-6a4 4 0 0 1 8 0v6"/>`},
	"loadbalancer": {"loadbalancer", "#db2777",
		`<circle cx="5" cy="12" r="2"/><circle cx="19" cy="5" r="2"/><circle cx="19" cy="12" r="2"/><circle cx="19" cy="19" r="2"/><path d="M7 12h2M11 12l6-6M11 12h6M11 12l6 6"/>`},
	"gateway": {"gateway", "#4f46e5",
		`<path d="M4 4v16M20 4v16"/><path d="M8 12h9M13 8l4 4-4 4"/>`},
	"service": {"service", "#0891b2",
		`<circle cx="12" cy="12" r="3"/><path d="M12 2v4M12 18v4M2 12h4M18 12h4M5 5l3 3M16 16l3 3M19 5l-3 3M8 16l-3 3"/>`},
	"compute": {"compute", "#475569",
		`<rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><path d="M7 7.5h.01M7 16.5h.01"/>`},
	"database": {"database", "#ea580c",
		`<ellipse cx="12" cy="5" rx="8" ry="3"/><path d="M4 5v14c0 1.66 3.58 3 8 3s8-1.34 8-3V5"/><path d="M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3"/>`},
	"storage": {"storage", "#0d9488",
		`<path d="M4 6h16l-1.5 13a2 2 0 0 1-2 1.8H7.5a2 2 0 0 1-2-1.8z"/><path d="M3 6h18"/>`},
	"network": {"network", "#0284c7",
		`<circle cx="6" cy="6" r="2.4"/><circle cx="18" cy="6" r="2.4"/><circle cx="12" cy="18" r="2.4"/><path d="M7.6 7.8l3 7.6M16.4 7.8l-3 7.6M8.4 6h7.2"/>`},
	"certificate": {"certificate", "#ca8a04",
		`<circle cx="12" cy="9" r="5"/><path d="M8.5 13l-1.5 8 5-3 5 3-1.5-8"/>`},
	"account": {"account", "#6b7280",
		`<path d="M4 21V5l8-3 8 3v16"/><path d="M4 21h16M9 9h.01M15 9h.01M9 13h.01M15 13h.01M10 21v-4h4v4"/>`},
	"workload": {"workload", "#2563eb",
		`<path d="M12 2l8 4.5v9L12 20l-8-4.5v-9z"/><path d="M12 2v18M12 11l8-4.5M12 11L4 6.5"/>`},
	"function": {"function", "#7c3aed",
		`<path d="M14 4h-2a3 3 0 0 0-3 3v3H6m3 0v7a3 3 0 0 1-3 3"/><path d="M9 12h6"/>`},
	"generic": {"generic", "#64748b",
		`<path d="M12 2l8.66 5v10L12 22l-8.66-5V7z"/>`},
}

// iconKeyForType maps an asset Type to one of the iconSet keys. Ordering
// matters: more specific substrings are checked before broad ones (e.g.
// "load_balancer" before the bare provider/network buckets). K8s Types look
// like "networking.k8s.io/v1.Ingress" — the Kind suffix carries the signal.
func iconKeyForType(provider, typ string) string {
	t := strings.ToLower(typ)

	switch {
	case strings.Contains(t, "dns"), strings.Contains(t, "zone"):
		return "dns"
	case strings.Contains(t, "certificate"), strings.Contains(t, "mtls"):
		return "certificate"
	case strings.Contains(t, "ruleset"), strings.Contains(t, "access"),
		strings.Contains(t, "waf"), strings.Contains(t, "page_rule"),
		strings.Contains(t, "networkpolicy"):
		return "waf"
	case strings.Contains(t, "tunnel"):
		return "tunnel"
	case strings.Contains(t, "load_balancer"), strings.Contains(t, "loadbalancer"),
		strings.HasSuffix(t, ".service") && strings.Contains(t, "lb"):
		return "loadbalancer"
	case strings.Contains(t, "ingress"), strings.Contains(t, "httproute"),
		strings.Contains(t, "gatewayclass"), strings.HasSuffix(t, ".gateway"),
		strings.Contains(t, "route"):
		return "gateway"
	case strings.HasSuffix(t, ".service"), strings.HasSuffix(t, ".endpoints"),
		strings.Contains(t, "oke"), strings.Contains(t, "cluster"):
		return "service"
	case strings.Contains(t, "function"), strings.Contains(t, "worker_script"),
		strings.Contains(t, "pages_project"):
		return "function"
	case strings.Contains(t, "database"), strings.Contains(t, "db_system"),
		strings.Contains(t, "kv_namespace"):
		return "database"
	case strings.Contains(t, "bucket"), strings.Contains(t, "object_storage"),
		strings.Contains(t, "volume"), strings.Contains(t, "r2_"),
		strings.Contains(t, "persistentvolume"), strings.Contains(t, "configmap"),
		strings.Contains(t, "secret"):
		return "storage"
	case strings.Contains(t, "vcn"), strings.Contains(t, "subnet"),
		strings.Contains(t, "_gateway"), strings.Contains(t, "drg"),
		strings.Contains(t, "peering"), strings.Contains(t, "vault"):
		return "network"
	case strings.Contains(t, "compute"), strings.Contains(t, "instance"),
		strings.HasSuffix(t, ".node"), strings.Contains(t, "container"):
		return "compute"
	case strings.HasSuffix(t, ".pod"), strings.Contains(t, "deployment"),
		strings.Contains(t, "replicaset"), strings.Contains(t, "statefulset"),
		strings.Contains(t, "daemonset"), strings.Contains(t, "application"),
		strings.Contains(t, "job"):
		return "workload"
	case strings.Contains(t, "account"), strings.Contains(t, "compartment"),
		strings.Contains(t, "iam"), strings.Contains(t, "user"),
		strings.Contains(t, "group"), strings.Contains(t, "policy"),
		strings.HasSuffix(t, ".namespace"), strings.Contains(t, "serviceaccount"):
		return "account"
	}
	_ = provider
	return "generic"
}

// iconForAsset returns the icon for an asset's Type.
func iconForAsset(a core.Asset) icon {
	return iconSet[iconKeyForType(a.Provider, a.Type)]
}

// dataURL renders the icon as a base64 SVG data URL — the form Excalidraw's
// `files` map expects.
func (i icon) dataURL() string {
	svg := svgDocument(i.color, i.body)
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}

// fileID is the stable Excalidraw file identifier for this icon. Content is
// fixed per key, so the ID stays constant across runs (determinism).
func (i icon) fileID() string {
	return excaliID("file", i.key)
}

// svgDocument wraps an icon body in a self-contained SVG with the stroke
// colour applied. No external refs, no <style> — Excalidraw rasterises it
// as-is.
func svgDocument(color, body string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="` +
		color + `" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">` +
		body + `</svg>`
}
