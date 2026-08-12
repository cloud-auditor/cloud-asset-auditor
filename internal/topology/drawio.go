package topology

import (
	"encoding/xml"
	"io"
	"sort"
	"strconv"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// drawioRenderer emits an mxGraph XML document (.drawio) openable in
// draw.io / diagrams.net for hand-editing. Node geometry reuses the
// left-to-right layered layout the Excalidraw renderer computes (layoutLR),
// so the diagram opens pre-arranged instead of as a pile at the origin;
// provider accent colours come from providerStyle. --group-by wraps nodes in
// labeled container cells (mxGraph child geometry is parent-relative, so
// member positions are rebased onto the container origin). Output is
// deterministic: sorted groups / nodes / edges, hash-derived cell IDs, and
// no timestamps — two renders of the same topology are byte-identical.
type drawioRenderer struct{ groupBy string }

// Container padding: side/bottom breathing room around member cards plus
// extra headroom at the top so the container label doesn't overlap them.
const (
	drawioGroupPad    = 24.0
	drawioGroupHeader = 40.0
)

type mxFile struct {
	XMLName xml.Name  `xml:"mxfile"`
	Host    string    `xml:"host,attr"`
	Agent   string    `xml:"agent,attr"`
	Version string    `xml:"version,attr"`
	Diagram mxDiagram `xml:"diagram"`
}

type mxDiagram struct {
	ID    string       `xml:"id,attr"`
	Name  string       `xml:"name,attr"`
	Model mxGraphModel `xml:"mxGraphModel"`
}

type mxGraphModel struct {
	Dx         string `xml:"dx,attr"`
	Dy         string `xml:"dy,attr"`
	Grid       string `xml:"grid,attr"`
	GridSize   string `xml:"gridSize,attr"`
	Guides     string `xml:"guides,attr"`
	Tooltips   string `xml:"tooltips,attr"`
	Connect    string `xml:"connect,attr"`
	Arrows     string `xml:"arrows,attr"`
	Fold       string `xml:"fold,attr"`
	Page       string `xml:"page,attr"`
	PageScale  string `xml:"pageScale,attr"`
	PageWidth  string `xml:"pageWidth,attr"`
	PageHeight string `xml:"pageHeight,attr"`
	Math       string `xml:"math,attr"`
	Shadow     string `xml:"shadow,attr"`
	Root       mxRoot `xml:"root"`
}

type mxRoot struct {
	Cells []mxCell `xml:"mxCell"`
}

// mxCell is both vertex and edge (mxGraph's model). vertex="1" / edge="1"
// discriminate; parent/source/target are cell-ID references. All are
// omitempty strings so the two mandatory root cells (id 0 and 1) stay bare,
// while set values — including "0" for connectable — always serialize.
type mxCell struct {
	ID          string      `xml:"id,attr"`
	Value       string      `xml:"value,attr,omitempty"`
	Style       string      `xml:"style,attr,omitempty"`
	Parent      string      `xml:"parent,attr,omitempty"`
	Source      string      `xml:"source,attr,omitempty"`
	Target      string      `xml:"target,attr,omitempty"`
	Vertex      string      `xml:"vertex,attr,omitempty"`
	Edge        string      `xml:"edge,attr,omitempty"`
	Connectable string      `xml:"connectable,attr,omitempty"`
	Geometry    *mxGeometry `xml:"mxGeometry,omitempty"`
}

type mxGeometry struct {
	X        string `xml:"x,attr,omitempty"`
	Y        string `xml:"y,attr,omitempty"`
	Width    string `xml:"width,attr,omitempty"`
	Height   string `xml:"height,attr,omitempty"`
	Relative string `xml:"relative,attr,omitempty"`
	As       string `xml:"as,attr"`
}

func (r drawioRenderer) Render(t *Topology, w io.Writer) error {
	layout := layoutLR(t)

	// The two mandatory mxGraph roots: cell 0 is the model root, cell 1 the
	// default layer. Every content cell parents to "1" (or to its container).
	cells := []mxCell{
		{ID: "0"},
		{ID: "1", Parent: "0"},
	}

	for _, g := range groupedNodes(t.Nodes, r.groupBy) {
		parent := "1"
		var originX, originY float64
		if g.Name != "" {
			// Container bounds from the members' absolute positions; member
			// geometry below is rebased to be relative to this origin.
			var minX, minY, maxX, maxY float64
			hasBounds := false
			for _, n := range g.Nodes {
				pos, ok := layout[refKey(n.AsRef())]
				if !ok {
					continue
				}
				if !hasBounds {
					minX, minY, maxX, maxY = pos.x, pos.y, pos.x, pos.y
					hasBounds = true
					continue
				}
				minX = min(minX, pos.x)
				minY = min(minY, pos.y)
				maxX = max(maxX, pos.x)
				maxY = max(maxY, pos.y)
			}
			if hasBounds {
				cid := "g_" + sanitizeID(g.Name) + "_" + shortHash(g.Name)
				originX = minX - drawioGroupPad
				originY = minY - drawioGroupHeader
				cells = append(cells, mxCell{
					ID:          cid,
					Value:       g.Name,
					Style:       "rounded=1;dashed=1;verticalAlign=top;html=0;fillColor=none;",
					Parent:      "1",
					Vertex:      "1",
					Connectable: "0",
					Geometry: &mxGeometry{
						X:      drawioNum(originX),
						Y:      drawioNum(originY),
						Width:  drawioNum(maxX - minX + boxWidth + 2*drawioGroupPad),
						Height: drawioNum(maxY - minY + boxHeight + drawioGroupHeader + drawioGroupPad),
						As:     "geometry",
					},
				})
				parent = cid
			}
		}
		for _, n := range g.Nodes {
			pos, ok := layout[refKey(n.AsRef())]
			if !ok {
				continue
			}
			stroke, fill := providerStyle(n.Provider)
			cells = append(cells, mxCell{
				ID:     graphNodeID(n.AsRef()),
				Value:  drawioLabel(n),
				Style:  "rounded=1;whiteSpace=wrap;html=0;fillColor=" + fill + ";strokeColor=" + stroke + ";",
				Parent: parent,
				Vertex: "1",
				Geometry: &mxGeometry{
					X:      drawioNum(pos.x - originX),
					Y:      drawioNum(pos.y - originY),
					Width:  drawioNum(boxWidth),
					Height: drawioNum(boxHeight),
					As:     "geometry",
				},
			})
		}
	}

	// Edges stay on the root layer even when their endpoints live inside
	// containers — mxGraph allows cross-parent connections.
	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	for i, e := range edges {
		style := "edgeStyle=orthogonalEdgeStyle;rounded=1;html=0;"
		if e.Confidence == core.ConfidenceHeuristic {
			// Dashed marks a cross-cloud heuristic join, mirroring the
			// dashed edges every other renderer uses.
			style += "dashed=1;"
		}
		cells = append(cells, mxCell{
			ID:       "e" + strconv.Itoa(i),
			Value:    edgeLabel(e),
			Style:    style,
			Parent:   "1",
			Source:   graphNodeID(e.From),
			Target:   graphNodeID(e.To),
			Edge:     "1",
			Geometry: &mxGeometry{Relative: "1", As: "geometry"},
		})
	}

	doc := mxFile{
		Host:    "cloud-asset-auditor",
		Agent:   "cloud-asset-auditor",
		Version: "1",
		Diagram: mxDiagram{
			ID:   "topology",
			Name: "Topology",
			Model: mxGraphModel{
				Dx: "800", Dy: "600",
				Grid: "0", GridSize: "10",
				Guides: "1", Tooltips: "1", Connect: "1", Arrows: "1", Fold: "1",
				Page: "1", PageScale: "1", PageWidth: "1600", PageHeight: "1200",
				Math: "0", Shadow: "0",
				Root: mxRoot{Cells: cells},
			},
		},
	}

	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	enc := xml.NewEncoder(w)
	enc.Indent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, err := io.WriteString(w, "\n")
	return err
}

// drawioLabel is the two-line vertex label: name on top (what the user scans
// for), the short type beneath. The "\n" survives as &#xA; in the value
// attribute and draw.io renders it as a line break (html=0 keeps the value
// plain text; whiteSpace=wrap handles long names).
func drawioLabel(a core.Asset) string {
	name := a.Name
	if name == "" {
		name = a.ID
	}
	if t := DisplayType(a); t != "" {
		return name + "\n" + shortType(t)
	}
	return name
}

// drawioNum formats a coordinate without trailing zeros ("40", not "40.000"),
// keeping the XML compact and deterministic.
func drawioNum(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}
