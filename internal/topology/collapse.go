package topology

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Detail levels select how much of the graph survives into the rendered
// diagram. They exist because one graph cannot serve both audiences: a
// low-level view of a real inventory is tens of thousands of nodes (useful
// for tracing one request path, unreadable as a network diagram), while a
// network diagram wants one box per platform.
const (
	// DetailLow renders every collected asset — the full, unmodified graph.
	// This is the default and the historical behaviour.
	DetailLow = "low"

	// DetailMedium collapses each group to one node per resource type:
	// "kubernetes · v1.Pod ×128". The middle ground that still shows what
	// kind of thing lives where.
	DetailMedium = "medium"

	// DetailHigh collapses each group to a single node — the executive
	// network diagram, one box per provider (or account, or region).
	DetailHigh = "high"
)

// CollapsedNodeType is the Type given to the synthetic nodes Collapse
// produces. Renderers and the UI key off it to style an aggregate
// differently from a real asset.
const CollapsedNodeType = "topology.group"

// ParseDetail validates a --detail flag value. Empty means DetailLow, so an
// unset flag keeps the historical full-graph behaviour. An unrecognised value
// is an error rather than a silent fallback: silently rendering 40,000 nodes
// when the user asked for a summary is the worst of the available outcomes.
func ParseDetail(s string) (string, error) {
	switch d := strings.ToLower(strings.TrimSpace(s)); d {
	case "", DetailLow:
		return DetailLow, nil
	case DetailMedium:
		return DetailMedium, nil
	case DetailHigh:
		return DetailHigh, nil
	default:
		return "", fmt.Errorf("unknown topology detail level %q (want low|medium|high)", s)
	}
}

// RenderGroupBy returns the dimension a renderer should actually cluster on,
// given the detail level the graph was collapsed at.
//
// At DetailHigh the collapse has already produced exactly one node per group,
// so asking the renderer to also cluster by that dimension wraps every node in
// a subgraph box of its own — a labelled rectangle around a labelled
// rectangle, which is pure noise. Every other level clusters as asked: at
// DetailMedium the per-type nodes genuinely benefit from being boxed by their
// provider.
func RenderGroupBy(detail, groupBy string) string {
	if detail == DetailHigh {
		return ""
	}
	return groupBy
}

// Collapse returns a summarised copy of the topology at the given detail
// level, bucketing nodes by dim ("provider", "account", or "region").
//
// DetailLow returns the receiver unchanged. For the other levels every node
// is replaced by the synthetic node for its bucket, and every edge is
// remapped onto those buckets:
//
//   - Edges that end up inside one bucket become self-loops. They are dropped
//     from the edge list — an arrow from a box to itself carries no
//     information in a network diagram — but they are not discarded: their
//     count lands on the bucket node's internal_edges tag, so "how much of
//     this platform's traffic is internal" survives the collapse.
//   - Edges that cross buckets are merged per (from, to, kind) with a Count.
//
// The result is deterministic: buckets and edges are emitted in sorted order,
// so two runs over the same inventory render byte-identically.
func (t *Topology) Collapse(level, dim string) *Topology {
	if level == "" || level == DetailLow {
		return t
	}
	if dim == "" {
		// Collapsing needs a bucketing dimension; provider is the one that
		// always has a value on every asset.
		dim = "provider"
	}

	// bucketKey → the nodes it swallowed, in first-seen order.
	members := map[string][]core.Asset{}
	// node refKey → bucketKey, so edges can be remapped in one pass.
	bucketOf := make(map[string]string, len(t.Nodes))
	var order []string

	for _, n := range t.Nodes {
		k := bucketKey(n, level, dim)
		if _, seen := members[k]; !seen {
			order = append(order, k)
		}
		members[k] = append(members[k], n)
		bucketOf[refKey(n.AsRef())] = k
	}
	sort.Strings(order)

	// Aggregate edges first — the internal-edge counts feed into the node
	// tags, so the nodes can't be built until the edges have been walked.
	type edgeKey struct{ from, to, kind string }
	agg := map[edgeKey]*core.Edge{}
	var edgeOrder []edgeKey
	internal := map[string]int{}

	for _, e := range t.Edges {
		from, fromOK := bucketOf[refKey(e.From)]
		to, toOK := bucketOf[refKey(e.To)]
		if !fromOK || !toOK {
			// An edge touching a node that isn't in the list (a filtered
			// graph) has nothing to collapse onto.
			continue
		}
		if from == to {
			internal[from]++
			continue
		}
		k := edgeKey{from, to, e.Kind}
		cur, ok := agg[k]
		if !ok {
			edgeOrder = append(edgeOrder, k)
			merged := core.Edge{Kind: e.Kind, Confidence: e.Confidence}
			agg[k] = &merged
			cur = &merged
		}
		cur.Count++
		// A summary arrow is only as trustworthy as its weakest constituent:
		// one heuristic match among ten exact ones means the aggregate is not
		// something to rely on.
		if e.Confidence == core.ConfidenceHeuristic {
			cur.Confidence = core.ConfidenceHeuristic
		}
	}

	out := &Topology{}
	refs := make(map[string]core.AssetRef, len(order))
	for _, k := range order {
		node := collapsedNode(k, level, dim, members[k], internal[k])
		refs[k] = node.AsRef()
		out.Nodes = append(out.Nodes, node)
	}

	sort.Slice(edgeOrder, func(i, j int) bool {
		a, b := edgeOrder[i], edgeOrder[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.to != b.to {
			return a.to < b.to
		}
		return a.kind < b.kind
	})
	for _, k := range edgeOrder {
		e := *agg[k]
		e.From = refs[k.from]
		e.To = refs[k.to]
		out.Edges = append(out.Edges, e)
	}
	return out
}

// bucketKey is the identity of the collapsed node a given asset folds into.
// The group name is prefixed with its length so a group called "a" holding a
// type "b:c" can't collide with a group "a:b" holding type "c".
func bucketKey(a core.Asset, level, dim string) string {
	g := groupOf(a, dim)
	if g == "" {
		g = a.Provider
	}
	if level == DetailHigh {
		return strconv.Itoa(len(g)) + ":" + g
	}
	return strconv.Itoa(len(g)) + ":" + g + "/" + a.Type
}

// collapsedNode builds the synthetic asset standing in for a bucket.
func collapsedNode(key, level, dim string, nodes []core.Asset, internalEdges int) core.Asset {
	group := groupOf(nodes[0], dim)
	if group == "" {
		group = nodes[0].Provider
	}

	name := fmt.Sprintf("%s ×%d", group, len(nodes))
	id := "group:" + dim + ":" + group
	if level == DetailMedium {
		name = fmt.Sprintf("%s · %s ×%d", group, nodes[0].Type, len(nodes))
		id += "/" + nodes[0].Type
	}

	// Provider is only meaningful when every member shares one; a
	// region-grouped bucket routinely spans providers, and claiming one of
	// them would mis-colour the node in every provider-aware renderer.
	provider := nodes[0].Provider
	account := nodes[0].AccountID
	region := nodes[0].Region
	for _, n := range nodes[1:] {
		if n.Provider != provider {
			provider = "multi"
		}
		if n.AccountID != account {
			account = ""
		}
		if n.Region != region {
			region = ""
		}
	}

	tags := map[string]string{
		"collapsed":    level,
		"group_by":     dim,
		"group":        group,
		"member_count": strconv.Itoa(len(nodes)),
	}
	if internalEdges > 0 {
		tags["internal_edges"] = strconv.Itoa(internalEdges)
	}
	if level == DetailMedium {
		tags["member_type"] = nodes[0].Type
	} else {
		tags["member_types"] = strings.Join(distinctTypes(nodes), ",")
	}

	return core.Asset{
		Provider:  provider,
		AccountID: account,
		Region:    region,
		Type:      CollapsedNodeType,
		ID:        id,
		Name:      name,
		Status:    "collapsed",
		Tags:      tags,
	}
}

// distinctTypes lists the asset types a bucket swallowed, sorted, so the
// summary node can say what it contains.
func distinctTypes(nodes []core.Asset) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, 8)
	for _, n := range nodes {
		if _, ok := seen[n.Type]; ok {
			continue
		}
		seen[n.Type] = struct{}{}
		out = append(out, n.Type)
	}
	sort.Strings(out)
	return out
}
