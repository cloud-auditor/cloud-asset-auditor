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
	elements, files := buildExcalidrawElements(t, layout)

	doc := map[string]any{
		"type":     "excalidraw",
		"version":  2,
		"source":   "https://github.com/cloud-auditor/cloud-asset-auditor",
		"elements": elements,
		"appState": map[string]any{
			"viewBackgroundColor":    "#ffffff",
			"currentItemFontFamily":  2, // Helvetica — legible, not the rough Virgil
			"currentItemStrokeColor": "#1f2328",
			"currentItemRoughness":   0, // crisp lines suit an architecture diagram
			"gridSize":               20,
		},
		"files": files,
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
	boxWidth  = 250.0
	boxHeight = 96.0
	hSpacing  = 360.0
	vSpacing  = 140.0
	marginX   = 40.0
	marginY   = 40.0

	iconSize = 40.0 // icon glyph, centred along the top of each box
	iconPad  = 10.0 // gap between the box top and the icon
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

// buildExcalidrawElements assembles rectangles, bound text, icon images, and
// bound arrows for the whole topology, plus the `files` map holding the
// base64 data URL for every icon actually used. Element IDs are derived from
// a 64-bit FNV hash of the asset ref / edge identity so output is stable
// across runs (Excalidraw doesn't care about ID content, only uniqueness).
func buildExcalidrawElements(t *Topology, layout map[string]position) ([]map[string]any, map[string]any) {
	type rectInfo struct {
		rectID  string
		textID  string
		boundTo []map[string]any // arrow refs to backfill into rect.boundElements
	}
	rects := map[string]*rectInfo{} // ref-key → rect info
	files := map[string]any{}       // fileId → Excalidraw file entry

	out := make([]map[string]any, 0, len(t.Nodes)*3+len(t.Edges))

	// Rectangle + icon image + bound text per node. The three share a
	// groupId so selecting/dragging one in Excalidraw moves the whole card.
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
		imageID := excaliID("img", k)
		groupID := excaliID("grp", k)
		rects[k] = &rectInfo{rectID: rectID, textID: textID}

		ic := iconForAsset(n)
		if _, seen := files[ic.fileID()]; !seen {
			files[ic.fileID()] = newFileEntry(ic)
		}

		out = append(out, newRect(rectID, textID, imageID, groupID, pos.x, pos.y, n))
		out = append(out, newImage(imageID, groupID, ic.fileID(), pos.x, pos.y))
		out = append(out, newText(textID, rectID, groupID, pos.x, pos.y, nodeLabel(n)))
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

	return out, files
}

// newRect creates a rectangle node element — the card body. The stroke
// carries the provider accent colour and the fill a light tint of it, so
// provider identity is readable while the icon carries service identity.
func newRect(id, textID, imageID, groupID string, x, y float64, a core.Asset) map[string]any {
	stroke, fill := providerStyle(a.Provider)
	return mergeCommon(id, "rectangle", x, y, boxWidth, boxHeight, map[string]any{
		"strokeColor":     stroke,
		"backgroundColor": fill,
		"fillStyle":       "solid",
		"strokeWidth":     2,
		"roughness":       0,
		"roundness":       map[string]any{"type": 3},
		"groupIds":        []string{groupID},
		// boundElements gets backfilled in buildExcalidrawElements. The icon
		// image is not container-bound (Excalidraw only binds text), it
		// tracks the card via the shared groupId instead.
		"boundElements": []map[string]any{{"id": textID, "type": "text"}},
	})
}

// newImage creates the icon glyph for a node, centred along the top of the
// card. It references an entry in the document's `files` map by fileId and
// shares the card's groupId so it moves with the box.
func newImage(id, groupID, fileID string, x, y float64) map[string]any {
	ix := x + (boxWidth-iconSize)/2
	iy := y + iconPad
	el := mergeCommon(id, "image", ix, iy, iconSize, iconSize, map[string]any{
		"groupIds": []string{groupID},
		"fileId":   fileID,
		"status":   "saved",
		"scale":    []float64{1, 1},
		"crop":     nil,
	})
	return el
}

// newText creates a text element bound inside a rectangle, anchored to the
// bottom of the card so it sits below the icon. The bound pattern is what
// Excalidraw uses for "text inside shape" — when the user moves the
// rectangle, the text follows.
func newText(id, containerID, groupID string, x, y float64, text string) map[string]any {
	// verticalAlign "bottom" keeps the label clear of the top-of-card icon;
	// Excalidraw recomputes the exact glyph position from the container, so
	// the x/y/width/height here just need to be sane bounds.
	return mergeCommon(id, "text", x+8, y+iconPad+iconSize, boxWidth-16, boxHeight-iconPad-iconSize-8, map[string]any{
		"strokeColor":   "#1f2328",
		"text":          text,
		"originalText":  text,
		"fontSize":      13,
		"fontFamily":    2, // Helvetica
		"textAlign":     "center",
		"verticalAlign": "bottom",
		"baseline":      13,
		"containerId":   containerID,
		"groupIds":      []string{groupID},
		// Bound text elements stay in sync with the container's
		// boundElements list — Excalidraw fixes any inconsistencies on
		// load, so omitting boundElements here is fine.
	})
}

// newFileEntry builds the document `files` map value for an icon: a base64
// SVG data URL plus the metadata Excalidraw expects. Timestamps are fixed at
// 0 so two renders of the same topology are byte-identical.
func newFileEntry(ic icon) map[string]any {
	return map[string]any{
		"mimeType":      "image/svg+xml",
		"id":            ic.fileID(),
		"dataURL":       ic.dataURL(),
		"created":       int64(0),
		"lastRetrieved": int64(0),
	}
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
		"roughness":       0,
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

// providerStyle picks a brand-ish accent stroke + light fill tint per
// provider so the graph is readable at a glance: the box says "which cloud",
// the icon says "which service".
func providerStyle(provider string) (stroke, fill string) {
	switch strings.ToLower(provider) {
	case "cloudflare":
		return "#f38020", "#fff4e6" // Cloudflare orange
	case "oci":
		return "#c74634", "#fdecea" // Oracle red
	case "kubernetes":
		return "#326ce5", "#e7f0ff" // Kubernetes blue
	default:
		return "#475569", "#f1f5f9" // slate
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
	if len(name) > 32 {
		name = name[:29] + "…"
	}
	// Name on the first line (what the user scans for), Type beneath it.
	return name + "\n" + shortType(a.Type)
}

// shortType drops the API-group/version prefix from a verbose Type so the
// label reads cleanly: "networking.k8s.io/v1.Ingress" → "Ingress",
// "oci.object_storage.bucket" → "bucket". Cloud Types keep their last
// dotted segment; that's enough to disambiguate next to the icon.
func shortType(typ string) string {
	if i := strings.LastIndex(typ, "."); i >= 0 && i < len(typ)-1 {
		return typ[i+1:]
	}
	return typ
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
