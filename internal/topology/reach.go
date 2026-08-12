package topology

import (
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
)

// Reachability analysis: given the graph Build produced, answer the questions
// an auditor actually asks — "what can reach this database?", "is production
// reachable from the internet?", "how exactly does A get to B?".
//
// Every answer is a *path*, not a yes/no, because the useful part of "yes, the
// internet can reach your database" is the hop list that makes it true.
//
// Direction matters and is the thing most easily got backwards, so it is
// spelled out here once: edges point the way a request travels. A DNS record
// points at its target, a load balancer at its backend, a policy rule from
// source to destination. So "what can reach X" walks edges *backwards* from X,
// and "what can X reach" walks them forwards.

// Path is one route through the graph: the nodes visited in order, and the
// edges traversed between them. len(Edges) == len(Nodes)-1.
type Path struct {
	Nodes []core.Asset `json:"nodes"`
	Edges []core.Edge  `json:"edges"`
}

// Hops is the number of edges traversed. A path to the node itself is 0 hops.
func (p Path) Hops() int { return len(p.Edges) }

// CrossesDeny reports whether any hop is a traffic-deny edge.
//
// A denial on the path does not necessarily mean the route is blocked — the
// graph records every policy statement, and precedence between them is an
// engine-specific question this package deliberately does not model. It means
// "a rule here says no", which is exactly the thing a human should look at.
func (p Path) CrossesDeny() bool {
	for _, e := range p.Edges {
		if e.Kind == core.EdgeKindTrafficDeny {
			return true
		}
	}
	return false
}

// ReachOptions tunes a traversal.
type ReachOptions struct {
	// MaxHops bounds path length. 0 means the default (6) — deep enough for
	// DNS → LB → gateway → service → pod → policy, shallow enough that a
	// dense graph doesn't produce paths nobody will read.
	MaxHops int

	// MaxPaths bounds how many paths a query returns — both the route
	// enumeration in Paths and the per-asset results from Reachable. 0 means
	// the default (25).
	//
	// It applies to both because the caller reports "truncated" from the
	// result count, and a bound that only held for one of the two made that
	// flag lie about the other: an exposure query over a large estate came
	// back complete-but-enormous, and was then labelled truncated purely for
	// being large. Both are breadth-first, so the retained paths are the
	// shortest ones.
	MaxPaths int

	// Kinds, when non-empty, restricts traversal to these edge kinds. Use it
	// to ask a policy-only question ("who may talk to whom") separately from
	// a plumbing question ("what points at what").
	Kinds []string

	// IncludeDeny keeps traffic-deny edges in the traversal. Off by default:
	// a deny edge is a statement that traffic does NOT flow, so following it
	// while computing reachability would manufacture routes that policy
	// forbids. Turn it on to audit the denials themselves.
	IncludeDeny bool
}

func (o ReachOptions) maxHops() int {
	if o.MaxHops <= 0 {
		return 6
	}
	return o.MaxHops
}

func (o ReachOptions) maxPaths() int {
	if o.MaxPaths <= 0 {
		return 25
	}
	return o.MaxPaths
}

// allows reports whether an edge may be traversed under these options.
func (o ReachOptions) allows(e core.Edge) bool {
	if e.Kind == core.EdgeKindTrafficDeny && !o.IncludeDeny {
		return false
	}
	if len(o.Kinds) == 0 {
		return true
	}
	for _, k := range o.Kinds {
		if k == e.Kind {
			return true
		}
	}
	return false
}

// graph is the adjacency view reachability walks. Built per query rather than
// cached on Topology: a Topology is a value callers filter and collapse, and a
// stale adjacency map attached to one would be a correctness trap.
type graph struct {
	nodes map[string]core.Asset
	out   map[string][]hop
	in    map[string][]hop
}

type hop struct {
	to   string
	edge core.Edge
}

func newGraph(t *Topology) *graph {
	g := &graph{
		nodes: make(map[string]core.Asset, len(t.Nodes)),
		out:   map[string][]hop{},
		in:    map[string][]hop{},
	}
	for _, n := range t.Nodes {
		g.nodes[refKey(n.AsRef())] = n
	}
	for _, e := range t.Edges {
		from, to := refKey(e.From), refKey(e.To)
		// An edge touching a node that was filtered out has no endpoint to
		// traverse to; skipping it keeps every path fully materialisable.
		if _, ok := g.nodes[from]; !ok {
			continue
		}
		if _, ok := g.nodes[to]; !ok {
			continue
		}
		g.out[from] = append(g.out[from], hop{to: to, edge: e})
		g.in[to] = append(g.in[to], hop{to: from, edge: e})
	}
	// Deterministic neighbour order → deterministic path enumeration, so the
	// same question asked twice returns the same answer in the same order.
	for _, m := range []map[string][]hop{g.out, g.in} {
		for k := range m {
			hops := m[k]
			sort.SliceStable(hops, func(i, j int) bool {
				if hops[i].to != hops[j].to {
					return hops[i].to < hops[j].to
				}
				return hops[i].edge.Kind < hops[j].edge.Kind
			})
		}
	}
	return g
}

// Direction of a traversal.
const (
	// Downstream follows edges forwards: what can this asset reach?
	Downstream = "downstream"
	// Upstream follows edges backwards: what can reach this asset?
	Upstream = "upstream"
)

// Select resolves a selector to the nodes it names. A selector is a
// case-insensitive glob (via internal/filter) matched against each node's ID
// and Name, so both `auditor reach --from api.example.com` and
// `--from 'ocid1.loadbalancer.*'` work without the caller saying which it is.
func (t *Topology) Select(selector string) []core.Asset {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return nil
	}
	var out []core.Asset
	for _, n := range t.Nodes {
		if filter.Glob(selector, n.ID) || filter.Glob(selector, n.Name) {
			out = append(out, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return refKey(out[i].AsRef()) < refKey(out[j].AsRef()) })
	return out
}

// Reachable returns every node reachable from the seeds, together with the
// shortest path to each. Direction is Downstream ("what can the seeds reach")
// or Upstream ("what can reach the seeds").
//
// Breadth-first, so the recorded path to each node is a shortest one. Seeds
// themselves are not included in the result — "what can X reach" should not
// answer "X".
func (t *Topology) Reachable(seeds []core.Asset, direction string, opts ReachOptions) []Path {
	g := newGraph(t)
	adj := g.out
	if direction == Upstream {
		adj = g.in
	}

	type queued struct {
		key  string
		path Path
	}

	seen := map[string]bool{}
	var queue []queued
	for _, s := range seeds {
		k := refKey(s.AsRef())
		if _, ok := g.nodes[k]; !ok || seen[k] {
			continue
		}
		seen[k] = true
		queue = append(queue, queued{key: k, path: Path{Nodes: []core.Asset{g.nodes[k]}}})
	}

	maxHops := opts.maxHops()
	maxPaths := opts.maxPaths()
	var out []Path
	for len(queue) > 0 && len(out) < maxPaths {
		cur := queue[0]
		queue = queue[1:]
		if cur.path.Hops() >= maxHops {
			continue
		}
		for _, h := range adj[cur.key] {
			if len(out) >= maxPaths {
				break
			}
			if seen[h.to] || !opts.allows(h.edge) {
				continue
			}
			seen[h.to] = true
			next := Path{
				// Copy rather than append-in-place: several queue entries
				// share a prefix slice, and appending to a shared backing
				// array would rewrite a sibling's path.
				Nodes: append(append([]core.Asset{}, cur.path.Nodes...), g.nodes[h.to]),
				Edges: append(append([]core.Edge{}, cur.path.Edges...), h.edge),
			}
			out = append(out, next)
			queue = append(queue, queued{key: h.to, path: next})
		}
	}
	return out
}

// Paths enumerates distinct simple routes from any source to any target,
// shortest first, capped by opts.MaxPaths.
//
// Simple means no node repeats within a path — without that, any cycle in the
// graph yields infinitely many routes that differ only by how many times they
// went round. Enumeration is breadth-first so the cap keeps the *shortest*
// paths rather than whichever ones a depth-first walk stumbled into.
func (t *Topology) Paths(sources, targets []core.Asset, opts ReachOptions) []Path {
	g := newGraph(t)

	want := map[string]bool{}
	for _, tgt := range targets {
		want[refKey(tgt.AsRef())] = true
	}
	if len(want) == 0 {
		return nil
	}

	maxHops := opts.maxHops()
	maxPaths := opts.maxPaths()

	var frontier []Path
	for _, s := range sources {
		k := refKey(s.AsRef())
		if _, ok := g.nodes[k]; !ok {
			continue
		}
		frontier = append(frontier, Path{Nodes: []core.Asset{g.nodes[k]}})
	}

	var out []Path
	for len(frontier) > 0 && len(out) < maxPaths {
		var next []Path
		for _, p := range frontier {
			if len(out) >= maxPaths {
				break
			}
			cur := refKey(p.Nodes[len(p.Nodes)-1].AsRef())

			// A source that is also a target is a zero-hop answer, but only
			// report it once, at depth 0.
			if want[cur] && p.Hops() > 0 {
				out = append(out, p)
				// Don't extend past a target: a route that reaches the target
				// and keeps going is a different question ("what's downstream
				// of the target"), not another way in.
				continue
			}
			if p.Hops() >= maxHops {
				continue
			}
			for _, h := range g.out[cur] {
				if !opts.allows(h.edge) || pathContains(p, h.to) {
					continue
				}
				next = append(next, Path{
					Nodes: append(append([]core.Asset{}, p.Nodes...), g.nodes[h.to]),
					Edges: append(append([]core.Edge{}, p.Edges...), h.edge),
				})
			}
		}
		frontier = next
	}
	return out
}

func pathContains(p Path, key string) bool {
	for _, n := range p.Nodes {
		if refKey(n.AsRef()) == key {
			return true
		}
	}
	return false
}

// ----------------------------------------------------------------------
// exposure
// ----------------------------------------------------------------------

// internetFacingTypes are the asset types treated as public entry points by
// EntryPoints. These are the places traffic originates from outside the
// estate, so anything downstream of one is, transitively, internet-reachable.
//
// This is a heuristic list and deliberately errs toward *over*-reporting: a
// public DNS record that resolves to nothing costs a reader one glance, while
// a missing entry point hides a whole exposed subtree. Under-scoping is the
// worse failure for a security question.
var internetFacingTypes = map[string]bool{
	"cloudflare.dns_record":     true,
	"cloudflare.load_balancer":  true,
	"cloudflare.tunnel":         true,
	"cloudflare.pages":          true,
	"oci.load_balancer":         true,
	"oci.network_load_balancer": true,
	"oci.internet_gateway":      true,
}

// EntryPoints returns the nodes the estate is entered through from outside.
//
// Two sources: the type list above, and any Kubernetes Service that a cloud
// has actually published — a Service only carries a
// status.loadBalancer.ingress address once a controller has provisioned a
// public address for it, which is stronger evidence of exposure than its
// declared spec.type. That check needs Raw (the topology CLI forces
// --include-raw on), and degrades to the type list without it.
func (t *Topology) EntryPoints() []core.Asset {
	seen := map[string]bool{}
	var out []core.Asset
	add := func(a core.Asset) {
		k := refKey(a.AsRef())
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, a)
	}

	for _, n := range t.Nodes {
		if internetFacingTypes[n.Type] {
			add(n)
			continue
		}
		if n.Provider == "kubernetes" && len(n.Raw) > 0 {
			if len(kubeExternalIPs(n.Raw)) > 0 || len(kubeExternalHosts(n.Raw)) > 0 {
				add(n)
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool { return refKey(out[i].AsRef()) < refKey(out[j].AsRef()) })
	return out
}

// Exposure is the result of an internet-reachability analysis.
type Exposure struct {
	// Entries are the public entry points the analysis started from.
	Entries []core.Asset `json:"entry_points"`
	// Paths is the shortest route from some entry point to each reachable
	// asset, one per reachable asset.
	Paths []Path `json:"paths"`
}

// Reached returns just the exposed assets, without their routes.
func (e Exposure) Reached() []core.Asset {
	out := make([]core.Asset, 0, len(e.Paths))
	for _, p := range e.Paths {
		out = append(out, p.Nodes[len(p.Nodes)-1])
	}
	return out
}

// Exposed computes what is reachable from the estate's public entry points.
//
// This is the "is production reachable from the internet?" question, and its
// answer is only as good as the graph: an asset is reported exposed when a
// path of collected relationships leads to it from an entry point. Absence of
// a path is not proof of isolation — it may equally mean a resolver could not
// join two providers, which is why every heuristic edge is marked as such.
func (t *Topology) Exposed(opts ReachOptions) Exposure {
	entries := t.EntryPoints()
	return Exposure{
		Entries: entries,
		Paths:   t.Reachable(entryRoots(t, entries, opts), Downstream, opts),
	}
}

// entryRoots reduces the entry-point set to those that are not themselves
// reachable from another entry point.
//
// This matters for the shape of the report. A published Kubernetes Service is
// an entry point by the type rules above, but it usually also sits *behind* a
// cloud load balancer that sits behind a DNS record — all three qualify. Seed
// the traversal with all of them and BFS marks the Service visited at depth 0,
// so the reported route to its pods starts at the Service and the actual way
// in from the internet (DNS → LB → Service) is never shown.
//
// Keeping only the roots gives every path a full chain from the outermost
// public surface. The rule is defensible on its own terms too: if X is
// reachable from Y, X is not a separate way in — Y is.
//
// Entries that are dropped here are still reported as entry points; they just
// appear as intermediate hops rather than as starting points.
func entryRoots(t *Topology, entries []core.Asset, opts ReachOptions) []core.Asset {
	if len(entries) < 2 {
		return entries
	}

	// Nodes reachable from some entry point via at least one edge. Computed
	// with a fresh traversal per entry so an entry is never disqualified by
	// its own presence in the seed set.
	downstream := map[string]bool{}
	for _, e := range entries {
		for _, p := range t.Reachable([]core.Asset{e}, Downstream, opts) {
			downstream[refKey(p.Nodes[len(p.Nodes)-1].AsRef())] = true
		}
	}

	var roots []core.Asset
	for _, e := range entries {
		if !downstream[refKey(e.AsRef())] {
			roots = append(roots, e)
		}
	}
	// A cycle among entry points can disqualify every one of them. Falling
	// back to the full set keeps the report populated — a slightly redundant
	// answer beats an empty one for a security question.
	if len(roots) == 0 {
		return entries
	}
	return roots
}
