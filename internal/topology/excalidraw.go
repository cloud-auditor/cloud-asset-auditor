package topology

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Excalidraw renderer. Produces a .excalidraw JSON document the user can
// drop into excalidraw.com (or the desktop app) to get an editable
// hand-drawn diagram with the request-path graph already laid out.
//
// Layout is longest-path layered LR (left-to-right): nodes with no
// incoming edges sit in layer 0, each subsequent node lands in
// max(predecessor_layer)+1. Within each layer, nodes are sorted by ref
// key for deterministic output. Arrows are bound to their endpoints so
// rearranging nodes in Excalidraw keeps the lines attached.

type excalidrawRenderer struct{}

func (excalidrawRenderer) Render(t *Topology, w io.Writer) error {
	layout := layoutLR(t)
	elements := buildExcalidrawElements(t, layout)

	doc := map[string]any{
		"type":    "excalidraw",
		"version": 2,
		"source":  "https://github.com/cloud-auditor/cloud-asset-auditor",
		"elements": elements,
		"appState": map[string]any{
			"viewBackgroundColor":     "#ffffff",
			"currentItemFontFamily":   2, // Helvetica — legible, not the rough Virgil
			"currentItemStrokeColor":  "#1f2328",
			"currentItemRoughness":    1,
			"gridSize":                20,
		},
		"files": map[string]any{},
	}

	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// ---------------------------------------------------------------------
// Layout
// ---------------------------------------------------------------------

const (
	boxWidth     = 240.0
	boxHeight    = 70.0
	hSpacing     = 350.0
	vSpacing     = 110.0
	marginX      = 40.0
	marginY      = 40.0
)

type position struct{ x, y float64 }

// layoutLR computes a left-to-right layered position for every node in t.
// The algorithm: compute predecessor counts, BFS from sources, assign
// layer = max(predecessor_layer)+1. Within each layer, sort by ref key
// and stack vertically. Pure layer assignment via longest-path is O(V+E)
// for a DAG; cycles (rare here) collapse to whatever layer they first
// reach, which is good-enough since renderers downstream don't choke.
func layoutLR(t *Topology) map[string]position {
	// Map node ref keys to themselves for fast lookups.
	ids := make([]string, 0, len(t.Nodes))
	nodeByID := make(map[string]core.AssetRef, len(t.Nodes))
	for _, n := range t.Nodes {
		k := refKey(n.AsRef())
		ids = append(ids, k)
		nodeByID[k] = n.AsRef()
	}

	preds := map[string]map[string]struct{}{}
	succs := map[string]map[string]struct{}{}
	for _, k := range ids {
		preds[k] = map[string]struct{}{}
		succs[k] = map[string]struct{}{}
	}
	for _, e := range t.Edges {
		f, to := refKey(e.From), refKey(e.To)
		if _, ok := preds[to]; !ok {
			continue
		}
		if _, ok := succs[f]; !ok {
			continue
		}
		preds[to][f] = struct{}{}
		succs[f][to] = struct{}{}
	}

	// Compute layer per node via longest-path. Initialize sources to 0.
	layer := make(map[string]int, len(ids))
	queue := make([]string, 0, len(ids))
	for _, k := range ids {
		if len(preds[k]) == 0 {
			layer[k] = 0
			queue = append(queue, k)
		}
	}
	visited := map[string]int{} // cycle guard — give up after revisits
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] > len(ids) {
			continue // cycle escape hatch
		}
		visited[cur]++
		for succ := range succs[cur] {
			candidate := layer[cur] + 1
			if existing, ok := layer[succ]; !ok || candidate > existing {
				layer[succ] = candidate
				queue = append(queue, succ)
			}
		}
	}
	// Any node not yet assigned (disconnected) goes to layer 0.
	for _, k := range ids {
		if _, ok := layer[k]; !ok {
			layer[k] = 0
		}
	}

	// Group by layer and sort within for deterministic vertical order.
	byLayer := map[int][]string{}
	for _, k := range ids {
		byLayer[layer[k]] = append(byLayer[layer[k]], k)
	}
	layers := make([]int, 0, len(byLayer))
	for l := range byLayer {
		layers = append(layers, l)
	}
	sort.Ints(layers)
	for _, l := range layers {
		sort.Strings(byLayer[l])
	}

	out := make(map[string]position, len(ids))
	for _, l := range layers {
		for i, k := range byLayer[l] {
			out[k] = position{
				x: marginX + float64(l)*hSpacing,
				y: marginY + float64(i)*vSpacing,
			}
		}
	}
	_ = nodeByID
	return out
}

// ---------------------------------------------------------------------
// Element construction
// ---------------------------------------------------------------------

// buildExcalidrawElements assembles rectangles, bound text, and bound
// arrows for the whole topology. Element IDs are derived from a 64-bit
// FNV hash of the asset ref / edge identity so output is stable across
// runs (Excalidraw doesn't care about ID content, only uniqueness).
func buildExcalidrawElements(t *Topology, layout map[string]position) []map[string]any {
	type rectInfo struct {
		rectID  string
		textID  string
		boundTo []map[string]any // arrow refs to backfill into rect.boundElements
	}
	rects := map[string]*rectInfo{} // ref-key → rect info

	out := make([]map[string]any, 0, len(t.Nodes)*2+len(t.Edges))

	// Rectangles + bound text per node.
	nodes := append([]core.Asset(nil), t.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return refKey(nodes[i].AsRef()) < refKey(nodes[j].AsRef())
	})
	for _, n := range nodes {
		k := refKey(n.AsRef())
		pos, ok := layout[k]
		if !ok {
			continue
		}
		rectID := excaliID("rect", k)
		textID := excaliID("text", k)
		rects[k] = &rectInfo{rectID: rectID, textID: textID}

		out = append(out, newRect(rectID, textID, pos.x, pos.y, n))
		out = append(out, newText(textID, rectID, pos.x, pos.y, nodeLabel(n)))
	}

	// Arrows.
	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	for _, e := range edges {
		fromKey := refKey(e.From)
		toKey := refKey(e.To)
		fr, frOk := rects[fromKey]
		to, toOk := rects[toKey]
		if !frOk || !toOk {
			continue
		}
		fpos, tpos := layout[fromKey], layout[toKey]
		arrowID := excaliID("arrow", fromKey+"->"+toKey+":"+e.Kind)

		boundRef := map[string]any{"id": arrowID, "type": "arrow"}
		fr.boundTo = append(fr.boundTo, boundRef)
		to.boundTo = append(to.boundTo, boundRef)

		out = append(out, newArrow(arrowID, fpos, tpos, fr.rectID, to.rectID, e))
	}

	// Backfill each rectangle's boundElements list now that we know which
	// arrows attach to it. Elements were appended in declaration order;
	// the rectangle is always the first of each pair, two positions back
	// from the corresponding text element.
	for _, el := range out {
		if el["type"] != "rectangle" {
			continue
		}
		id, _ := el["id"].(string)
		// Find the matching rect record by ID.
		for _, info := range rects {
			if info.rectID != id {
				continue
			}
			binds := []map[string]any{
				{"id": info.textID, "type": "text"},
			}
			binds = append(binds, info.boundTo...)
			el["boundElements"] = binds
			break
		}
	}

	return out
}

// newRect creates a rectangle node element.
func newRect(id, textID string, x, y float64, a core.Asset) map[string]any {
	return mergeCommon(id, "rectangle", x, y, boxWidth, boxHeight, map[string]any{
		"strokeColor":     "#1f2328",
		"backgroundColor": providerFill(a.Provider),
		"fillStyle":       "solid",
		"strokeWidth":     2,
		"roughness":       1,
		"roundness":       map[string]any{"type": 3},
		// boundElements gets backfilled in buildExcalidrawElements.
		"boundElements": []map[string]any{{"id": textID, "type": "text"}},
	})
}

// newText creates a text element bound inside a rectangle. The bound
// pattern is what Excalidraw uses for "text inside shape" — when the
// user moves the rectangle, the text follows.
func newText(id, containerID string, x, y float64, text string) map[string]any {
	// Text is centered inside the box; Excalidraw expects the text element
	// to share the container's bounds for centered placement.
	return mergeCommon(id, "text", x+8, y+8, boxWidth-16, boxHeight-16, map[string]any{
		"strokeColor":   "#1f2328",
		"text":          text,
		"originalText":  text,
		"fontSize":      14,
		"fontFamily":    2, // Helvetica
		"textAlign":     "center",
		"verticalAlign": "middle",
		"baseline":      14,
		"containerId":   containerID,
		// Bound text elements stay in sync with the container's
		// boundElements list — Excalidraw fixes any inconsistencies on
		// load, so omitting boundElements here is fine.
	})
}

// newArrow creates an arrow with start/end bindings to the source and
// target rectangles. Dashed when the edge confidence is heuristic so
// users immediately see which edges are guesses.
func newArrow(id string, from, to position, fromRect, toRect string, e core.Edge) map[string]any {
	startX := from.x + boxWidth
	startY := from.y + boxHeight/2
	endX := to.x
	endY := to.y + boxHeight/2
	dx := endX - startX
	dy := endY - startY

	style := "solid"
	stroke := "#1f2328"
	if e.Confidence == core.ConfidenceHeuristic {
		style = "dashed"
		stroke = "#8b949e"
	}

	return mergeCommon(id, "arrow", startX, startY, dx, dy, map[string]any{
		"strokeColor":     stroke,
		"backgroundColor": "transparent",
		"fillStyle":       "solid",
		"strokeWidth":     2,
		"strokeStyle":     style,
		"roughness":       1,
		"points":          [][2]float64{{0, 0}, {dx, dy}},
		"startBinding":    map[string]any{"elementId": fromRect, "focus": 0.0, "gap": 4},
		"endBinding":      map[string]any{"elementId": toRect, "focus": 0.0, "gap": 4},
		"startArrowhead":  nil,
		"endArrowhead":    "arrow",
		"label":           edgeLabel(e), // not native Excalidraw; ignored on load but useful for grep
	})
}

// mergeCommon fills the always-required fields shared by every
// Excalidraw element (the schema is permissive about defaults but expects
// these to be present).
func mergeCommon(id, kind string, x, y, w, h float64, extra map[string]any) map[string]any {
	seed := stableSeed(id)
	el := map[string]any{
		"id":              id,
		"type":            kind,
		"x":               x,
		"y":               y,
		"width":           w,
		"height":          h,
		"angle":           0.0,
		"strokeColor":     "#1f2328",
		"backgroundColor": "transparent",
		"fillStyle":       "hachure",
		"strokeWidth":     1,
		"strokeStyle":     "solid",
		"roughness":       1,
		"opacity":         100,
		"groupIds":        []string{},
		"frameId":         nil,
		"roundness":       nil,
		"seed":            seed,
		"version":         1,
		"versionNonce":    seed,
		"isDeleted":       false,
		"boundElements":   []map[string]any{},
		"updated":         int64(0),
		"link":            nil,
		"locked":          false,
	}
	for k, v := range extra {
		el[k] = v
	}
	return el
}

// providerFill picks a brand-ish background tint per provider so the
// graph is readable at a glance.
func providerFill(provider string) string {
	switch strings.ToLower(provider) {
	case "cloudflare":
		return "#fef3c7" // amber-100
	case "oci":
		return "#fee2e2" // red-100
	case "kubernetes":
		return "#dbeafe" // blue-100
	default:
		return "#f3f4f6" // gray-100
	}
}

// nodeLabel is the visible text inside a rectangle. Keep it compact —
// the box is 240×70 and Excalidraw doesn't auto-wrap nicely.
func nodeLabel(a core.Asset) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	// Trim long names so they fit in the box.
	if len(name) > 28 {
		name = name[:25] + "…"
	}
	return a.Type + "\n" + name
}

// excaliID derives a deterministic short element ID from a category +
// per-element identity. Same input → same ID, which keeps re-renders of
// the same topology stable across runs (useful for diffs).
func excaliID(prefix, identity string) string {
	h := fnv.New64a()
	_, _ = io.WriteString(h, prefix+"|"+identity)
	return fmt.Sprintf("%s_%x", prefix, h.Sum64())
}

// stableSeed produces a small positive int from a string, used as the
// per-element seed Excalidraw uses for its hand-drawn-look randomness.
// Deterministic so two runs of the same topology produce the same SVG.
func stableSeed(s string) int {
	h := fnv.New32a()
	_, _ = io.WriteString(h, s)
	return int(h.Sum32() & 0x7fffffff)
}
