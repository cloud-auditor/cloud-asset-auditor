package output

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"text/template"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/version"
)

// htmlTemplate is the report page: inline CSS and JS, zero external requests,
// with the audit data injected as one JSON blob (see reportData). Charts,
// summary cards, and the sortable/filterable table are all built client-side
// from that blob, keeping the Go side to "marshal, execute template".
//
//go:embed html_template.html
var htmlTemplate string

// text/template, not html/template: the only dynamic values are the JSON
// payload (already made script-safe by encoding/json's default <, >, &
// escaping) and three header scalars we escape explicitly. html/template's
// contextual auto-escaper would have to lex the large inline script and buys
// nothing here.
var htmlTmpl = template.Must(template.New("report").Parse(htmlTemplate))

// HTML renders assets as one fully self-contained report page — suitable for
// attaching to a ticket or emailing around without a web server.
//
// Like XLSX — and as the second sanctioned exception to the project's
// "stream end-to-end" invariant — HTML must buffer the entire stream: the
// charts and summary counts are derived from the full asset set, and the
// payload is emitted as a single blob. Memory is O(total assets); for very
// large inventories prefer CSV or NDJSON.
type HTML struct {
	// Now stamps the "generated at" header; nil means time.Now. A knob so
	// tests (and the golden file) can pin the timestamp.
	Now func() time.Time
}

var _ Renderer = (*HTML)(nil)

// htmlAsset is the slice of core.Asset the report needs. CreatedAt and Raw
// are deliberately dropped: the table doesn't show them, and Raw can be
// megabytes of provider payload a shareable report shouldn't carry (or leak).
type htmlAsset struct {
	Provider  string            `json:"provider"`
	AccountID string            `json:"account_id"`
	Region    string            `json:"region,omitempty"`
	Type      string            `json:"type"`
	ID        string            `json:"id"`
	Name      string            `json:"name"`
	Status    string            `json:"status,omitempty"`
	Tags      map[string]string `json:"tags,omitempty"`
}

// reportData is the embedded JSON payload the inline JS consumes via
// JSON.parse(document.getElementById("report-data").textContent).
type reportData struct {
	GeneratedAt string      `json:"generated_at"`
	Version     string      `json:"version"`
	Total       int         `json:"total"`
	Assets      []htmlAsset `json:"assets"`
}

// htmlTemplateData is what the template actually sees: header scalars
// pre-escaped for an HTML context, and the payload pre-escaped by the JSON
// encoder for a <script> context.
type htmlTemplateData struct {
	GeneratedAt string
	Version     string
	Total       int
	DataJSON    string
}

func (r *HTML) Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error {
	assets, err := drain(ctx, in)
	if err != nil {
		return err
	}

	now := time.Now
	if r.Now != nil {
		now = r.Now
	}

	rows := make([]htmlAsset, 0, len(assets))
	for _, a := range assets {
		rows = append(rows, htmlAsset{
			Provider:  a.Provider,
			AccountID: a.AccountID,
			Region:    a.Region,
			Type:      a.Type,
			ID:        a.ID,
			Name:      a.Name,
			Status:    a.Status,
			Tags:      a.Tags,
		})
	}

	payload := reportData{
		GeneratedAt: now().UTC().Format(time.RFC3339),
		Version:     version.Version,
		Total:       len(rows),
		Assets:      rows,
	}
	// json.Marshal's default escaping turns < > & into \u00XX, so the blob
	// can never contain "</script>" and is safe to inject verbatim into the
	// script tag. (Map keys sort during marshal, so Tags can't leak map
	// iteration order — identical input plus a fixed Now is byte-identical.)
	blob, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode report data: %w", err)
	}

	bw := bufio.NewWriter(w)
	data := htmlTemplateData{
		GeneratedAt: html.EscapeString(payload.GeneratedAt),
		Version:     html.EscapeString(payload.Version),
		Total:       payload.Total,
		DataJSON:    string(blob),
	}
	if err := htmlTmpl.Execute(bw, data); err != nil {
		return fmt.Errorf("render html: %w", err)
	}
	return bw.Flush()
}
