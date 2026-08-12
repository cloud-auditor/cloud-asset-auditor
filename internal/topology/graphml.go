package topology

import (
	"encoding/xml"
	"io"
	"sort"
	"strconv"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// graphmlRenderer emits GraphML (http://graphml.graphdrawing.org/) — the XML
// interchange format read by yEd, Gephi, and Cytoscape desktop. The graph is
// flat; provider / account / region / type / status ride along as node
// <data> attributes so those tools can color, filter, or auto-group by any of
// them (richer than a fixed cluster dimension, which is why this renderer
// doesn't take --group-by). Output is deterministic: nodes and edges are
// sorted and <key> declarations have a fixed order via struct fields.
type graphmlRenderer struct{}

type graphmlDoc struct {
	XMLName xml.Name     `xml:"graphml"`
	XMLNS   string       `xml:"xmlns,attr"`
	Keys    []graphmlKey `xml:"key"`
	Graph   graphmlGraph `xml:"graph"`
}

type graphmlKey struct {
	ID       string `xml:"id,attr"`
	For      string `xml:"for,attr"`
	AttrName string `xml:"attr.name,attr"`
	AttrType string `xml:"attr.type,attr"`
}

type graphmlGraph struct {
	ID          string        `xml:"id,attr"`
	EdgeDefault string        `xml:"edgedefault,attr"`
	Nodes       []graphmlNode `xml:"node"`
	Edges       []graphmlEdge `xml:"edge"`
}

type graphmlNode struct {
	ID   string        `xml:"id,attr"`
	Data []graphmlData `xml:"data"`
}

type graphmlEdge struct {
	ID     string        `xml:"id,attr"`
	Source string        `xml:"source,attr"`
	Target string        `xml:"target,attr"`
	Data   []graphmlData `xml:"data"`
}

type graphmlData struct {
	Key   string `xml:"key,attr"`
	Value string `xml:",chardata"`
}

func (graphmlRenderer) Render(t *Topology, w io.Writer) error {
	doc := graphmlDoc{
		XMLNS: "http://graphml.graphdrawing.org/xmlns",
		Keys: []graphmlKey{
			{ID: "label", For: "node", AttrName: "label", AttrType: "string"},
			{ID: "type", For: "node", AttrName: "type", AttrType: "string"},
			{ID: "provider", For: "node", AttrName: "provider", AttrType: "string"},
			{ID: "account", For: "node", AttrName: "account", AttrType: "string"},
			{ID: "region", For: "node", AttrName: "region", AttrType: "string"},
			{ID: "status", For: "node", AttrName: "status", AttrType: "string"},
			{ID: "kind", For: "edge", AttrName: "kind", AttrType: "string"},
			{ID: "confidence", For: "edge", AttrName: "confidence", AttrType: "string"},
			{ID: "hostname", For: "edge", AttrName: "hostname", AttrType: "string"},
			{ID: "port", For: "edge", AttrName: "port", AttrType: "int"},
		},
		Graph: graphmlGraph{ID: "topology", EdgeDefault: "directed"},
	}

	nodes := append([]core.Asset(nil), t.Nodes...)
	sort.Slice(nodes, func(i, j int) bool {
		return refKey(nodes[i].AsRef()) < refKey(nodes[j].AsRef())
	})
	for _, n := range nodes {
		name := n.Name
		if name == "" {
			name = n.ID
		}
		data := []graphmlData{
			{Key: "label", Value: name},
			{Key: "type", Value: n.Type},
			{Key: "provider", Value: n.Provider},
		}
		if n.AccountID != "" {
			data = append(data, graphmlData{Key: "account", Value: n.AccountID})
		}
		if n.Region != "" {
			data = append(data, graphmlData{Key: "region", Value: n.Region})
		}
		if n.Status != "" {
			data = append(data, graphmlData{Key: "status", Value: n.Status})
		}
		doc.Graph.Nodes = append(doc.Graph.Nodes, graphmlNode{ID: dotID(n.AsRef()), Data: data})
	}

	edges := append([]core.Edge(nil), t.Edges...)
	sort.Slice(edges, func(i, j int) bool {
		ki := refKey(edges[i].From) + refKey(edges[i].To) + edges[i].Kind
		kj := refKey(edges[j].From) + refKey(edges[j].To) + edges[j].Kind
		return ki < kj
	})
	for i, e := range edges {
		data := []graphmlData{
			{Key: "kind", Value: e.Kind},
			{Key: "confidence", Value: e.Confidence},
		}
		if e.Hostname != "" {
			data = append(data, graphmlData{Key: "hostname", Value: e.Hostname})
		}
		if e.Port != 0 {
			data = append(data, graphmlData{Key: "port", Value: strconv.Itoa(e.Port)})
		}
		doc.Graph.Edges = append(doc.Graph.Edges, graphmlEdge{
			ID:     "e" + strconv.Itoa(i),
			Source: dotID(e.From),
			Target: dotID(e.To),
			Data:   data,
		})
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
