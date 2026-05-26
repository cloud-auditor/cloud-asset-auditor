// Package topology infers the request-path graph between collected
// assets. Phase 10 of init-plan.md.
//
// Inputs are the flat []Asset that providers produce; outputs are
// []Edge derived by per-kind resolver functions running over an index
// keyed by IP and hostname. The package is read-only — it never modifies
// the asset list it's given.
package topology

import (
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Topology is the result of Build: every input asset plus every inferred
// edge. Renderers consume this directly.
type Topology struct {
	Nodes []core.Asset `json:"nodes"`
	Edges []core.Edge  `json:"edges"`
}

// Build runs every registered resolver over the asset list and returns the
// resulting graph. Resolvers see a shared Index so repeated lookups stay
// O(1) — naive O(n²) joins fall over fast against real inventories.
func Build(assets []core.Asset) *Topology {
	idx := newIndex(assets)

	var edges []core.Edge
	for _, r := range resolvers {
		edges = append(edges, r(idx)...)
	}
	edges = dedupEdges(edges)

	return &Topology{
		Nodes: assets,
		Edges: edges,
	}
}

// FilterByHostname keeps only nodes reachable (in either direction) from
// the connected component of any DNS record whose Name matches one of
// hostnames. Edges between kept nodes are kept; edges touching dropped
// nodes are removed.
//
// Hostname matching is case-insensitive and exact on the record's Name
// field (or its CNAME content for indirect matches via the index).
func (t *Topology) FilterByHostname(hostnames []string) *Topology {
	if len(hostnames) == 0 {
		return t
	}

	wanted := make(map[string]struct{}, len(hostnames))
	for _, h := range hostnames {
		wanted[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
	}

	// Build adjacency from the existing edges so we can BFS to the
	// connected component.
	adj := map[string]map[string]struct{}{}
	for _, e := range t.Edges {
		fromKey := refKey(e.From)
		toKey := refKey(e.To)
		if adj[fromKey] == nil {
			adj[fromKey] = map[string]struct{}{}
		}
		if adj[toKey] == nil {
			adj[toKey] = map[string]struct{}{}
		}
		adj[fromKey][toKey] = struct{}{}
		adj[toKey][fromKey] = struct{}{}
	}

	// Seed: every DNS record whose Name matches one of the wanted hostnames.
	keep := map[string]struct{}{}
	queue := []string{}
	for _, a := range t.Nodes {
		if a.Type != "cloudflare.dns_record" {
			continue
		}
		if _, ok := wanted[strings.ToLower(a.Name)]; ok {
			k := refKey(a.AsRef())
			keep[k] = struct{}{}
			queue = append(queue, k)
		}
	}

	// BFS the adjacency closure so we keep transitive matches too.
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for nbr := range adj[cur] {
			if _, ok := keep[nbr]; ok {
				continue
			}
			keep[nbr] = struct{}{}
			queue = append(queue, nbr)
		}
	}

	out := &Topology{}
	for _, a := range t.Nodes {
		if _, ok := keep[refKey(a.AsRef())]; ok {
			out.Nodes = append(out.Nodes, a)
		}
	}
	for _, e := range t.Edges {
		_, fOk := keep[refKey(e.From)]
		_, tOk := keep[refKey(e.To)]
		if fOk && tOk {
			out.Edges = append(out.Edges, e)
		}
	}
	return out
}

// DropOrphans removes nodes that have no incident edges. The CLI sets
// `--include-orphans` to flip this off (default behavior IS to drop).
func (t *Topology) DropOrphans() *Topology {
	used := map[string]struct{}{}
	for _, e := range t.Edges {
		used[refKey(e.From)] = struct{}{}
		used[refKey(e.To)] = struct{}{}
	}
	out := &Topology{Edges: t.Edges}
	for _, a := range t.Nodes {
		if _, ok := used[refKey(a.AsRef())]; ok {
			out.Nodes = append(out.Nodes, a)
		}
	}
	return out
}

// refKey is the canonical map-key string for an AssetRef. provider+id is
// unique in practice; if two providers ever collide on id, the resulting
// false-merge is preferable to silently dropping one.
func refKey(r core.AssetRef) string {
	return r.Provider + "/" + r.ID
}

// dedupEdges collapses identical (From, To, Kind) triples that different
// resolvers might both have produced. Keeps the first occurrence; that
// preserves whichever Confidence the earlier-running resolver assigned —
// resolver registration order is therefore a priority order for ties.
func dedupEdges(edges []core.Edge) []core.Edge {
	seen := map[string]struct{}{}
	out := edges[:0]
	for _, e := range edges {
		k := refKey(e.From) + "→" + refKey(e.To) + ":" + e.Kind
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, e)
	}
	return out
}
