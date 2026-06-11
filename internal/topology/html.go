package topology

import (
	"bufio"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"text/template"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// HTML renderer. Produces one fully self-contained page with the same
// force-directed SVG viewer the embedded web UI's Topology tab has —
// inline CSS, inline JS, zero external requests — so
// `auditor topology -o html > topology.html` yields a file that can be
// mailed around and opened offline in any browser.
//
// The viewer JS in html_template.html is an adaptation of
// internal/server/web/topology.js with the server-bound plumbing
// (fetch/build/cancel, exports, window.auditorShared, demo mode) stripped;
// keep the physics and interaction code in sync when touching either.

// htmlTemplate is the standalone viewer page. The graph is injected as one
// JSON blob in a <script type="application/json"> data island (see
// htmlGraph) — the same house pattern as internal/output/html_template.html.
//
//go:embed html_template.html
var htmlTemplate string

// text/template, not html/template: the only dynamic values are the JSON
// payload (already made script-safe by encoding/json's default <, >, &
// escaping) and two integer counts. html/template's contextual auto-escaper
// would have to lex the large inline script and buys nothing here.
var htmlTmpl = template.Must(template.New("topology").Parse(htmlTemplate))

type htmlRenderer struct{}

// htmlGraph is the embedded JSON payload the inline JS consumes via
// JSON.parse(document.getElementById("topology-data").textContent). Same
// {nodes, edges} shape as the server's /api/v1/topology JSON envelope.
type htmlGraph struct {
	Nodes []core.Asset `json:"nodes"`
	Edges []core.Edge  `json:"edges"`
}

// htmlTemplateData is what the template actually sees: the payload
// pre-escaped by the JSON encoder for a <script> context, plus the counts
// for the header bar (static, so they read correctly even with JS off).
type htmlTemplateData struct {
	NodeCount int
	EdgeCount int
	DataJSON  string
}

// Render writes the standalone page. Output is deterministic: same
// Topology → byte-identical file. json.Marshal sorts map keys (Tags), node
// and edge order is preserved from the input (Build's output is already
// stable), and the page carries no timestamps or randomness.
func (htmlRenderer) Render(t *Topology, w io.Writer) error {
	// Drop Asset.Raw on the way out — same reasoning as the JSON renderer
	// and the server envelope: the resolvers already extracted what they
	// needed, and a shareable file shouldn't carry (or leak) megabytes of
	// provider payload.
	stripped := make([]core.Asset, len(t.Nodes))
	for i, a := range t.Nodes {
		a.Raw = nil
		stripped[i] = a
	}
	edges := t.Edges
	if edges == nil {
		edges = []core.Edge{} // marshal as [], not null — keeps the island shape regular
	}

	// json.Marshal's default escaping turns < > & into \u00XX, so the blob
	// can never contain "</script>" and is safe to inject verbatim into the
	// script tag.
	blob, err := json.Marshal(htmlGraph{Nodes: stripped, Edges: edges})
	if err != nil {
		return fmt.Errorf("encode topology data: %w", err)
	}

	bw := bufio.NewWriter(w)
	data := htmlTemplateData{
		NodeCount: len(t.Nodes),
		EdgeCount: len(t.Edges),
		DataJSON:  string(blob),
	}
	if err := htmlTmpl.Execute(bw, data); err != nil {
		return fmt.Errorf("render html: %w", err)
	}
	return bw.Flush()
}
