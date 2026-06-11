package output_test

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/version"
)

// htmlNow pins the report timestamp so the golden bytes are reproducible.
func htmlNow() time.Time { return time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC) }

func renderHTML(t *testing.T, assets []core.Asset) []byte {
	t.Helper()
	var buf bytes.Buffer
	r := &output.HTML{Now: htmlNow}
	if err := r.Render(context.Background(), feedAssets(assets), &buf); err != nil {
		t.Fatalf("render: %v", err)
	}
	return buf.Bytes()
}

func TestHTML_Golden(t *testing.T) {
	assertGolden(t, "html.golden", renderHTML(t, fixtureAssets(t)))
}

// reportPayload mirrors the embedded JSON blob the inline JS consumes.
type reportPayload struct {
	GeneratedAt string `json:"generated_at"`
	Version     string `json:"version"`
	Total       int    `json:"total"`
	Assets      []struct {
		Provider  string            `json:"provider"`
		AccountID string            `json:"account_id"`
		Region    string            `json:"region"`
		Type      string            `json:"type"`
		ID        string            `json:"id"`
		Name      string            `json:"name"`
		Status    string            `json:"status"`
		Tags      map[string]string `json:"tags"`
	} `json:"assets"`
}

// extractReportData slices the embedded payload out of the rendered document.
// json.Marshal's default escaping guarantees the payload itself can never
// contain "</script>", so the first closer after the marker is the real one.
func extractReportData(t *testing.T, doc []byte) []byte {
	t.Helper()
	const open = `<script id="report-data" type="application/json">`
	i := bytes.Index(doc, []byte(open))
	if i < 0 {
		t.Fatal("report-data script tag not found")
	}
	rest := doc[i+len(open):]
	j := bytes.Index(rest, []byte("</script>"))
	if j < 0 {
		t.Fatal("report-data script tag not closed")
	}
	return rest[:j]
}

func TestHTML_PayloadRoundTrip(t *testing.T) {
	doc := renderHTML(t, fixtureAssets(t))

	var got reportPayload
	if err := json.Unmarshal(extractReportData(t, doc), &got); err != nil {
		t.Fatalf("embedded payload is not valid JSON: %v", err)
	}

	if got.GeneratedAt != "2025-01-02T03:04:05Z" {
		t.Errorf("generated_at = %q, want 2025-01-02T03:04:05Z", got.GeneratedAt)
	}
	if got.Version != version.Version {
		t.Errorf("version = %q, want %q", got.Version, version.Version)
	}
	if got.Total != 2 || len(got.Assets) != 2 {
		t.Fatalf("total = %d, assets = %d, want 2 and 2", got.Total, len(got.Assets))
	}

	a := got.Assets[0]
	if a.Provider != "cloudflare" || a.AccountID != "acct1" || a.Type != "cloudflare.zone" ||
		a.ID != "zone-1" || a.Name != "example.com" || a.Status != "active" {
		t.Errorf("asset 0 fields did not round-trip: %+v", a)
	}
	if want := map[string]string{"env": "prod", "team": "platform"}; !reflect.DeepEqual(a.Tags, want) {
		t.Errorf("asset 0 tags = %v, want %v", a.Tags, want)
	}
	if b := got.Assets[1]; b.Provider != "kubernetes" || b.Region != "local" || b.Name != "nginx-abc" {
		t.Errorf("asset 1 fields did not round-trip: %+v", b)
	}
}

func TestHTML_ScriptCloserInAssetCannotBreakOut(t *testing.T) {
	hostile := `</script><script>alert(1)</script>`
	doc := renderHTML(t, []core.Asset{{
		Provider: "x", AccountID: "y", Type: "t", ID: "i", Name: hostile,
	}})

	// The closer must arrive \u-escaped, never verbatim...
	if bytes.Contains(doc, []byte(hostile)) {
		t.Error("hostile asset name embedded unescaped")
	}
	// ...and still round-trip to the original string.
	var got reportPayload
	if err := json.Unmarshal(extractReportData(t, doc), &got); err != nil {
		t.Fatalf("embedded payload is not valid JSON: %v", err)
	}
	if len(got.Assets) != 1 || got.Assets[0].Name != hostile {
		t.Errorf("hostile name did not round-trip: %+v", got.Assets)
	}
}

func TestHTML_Deterministic(t *testing.T) {
	// Tags maps are the map-iteration hazard; enough keys that an order leak
	// would actually flip between two renders in the same process.
	assets := []core.Asset{{
		Provider: "x", AccountID: "y", Type: "t", ID: "i", Name: "n",
		Tags: map[string]string{
			"a": "1", "b": "2", "c": "3", "d": "4", "e": "5",
			"f": "6", "g": "7", "h": "8", "i": "9", "j": "10",
		},
	}}
	first := renderHTML(t, assets)
	second := renderHTML(t, assets)
	if !bytes.Equal(first, second) {
		t.Error("two renders of identical input differ — nondeterminism leak")
	}
}

func TestHTML_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	src := make(chan core.Asset)
	// Never close src; rely on the ctx already being cancelled.
	if err := (&output.HTML{}).Render(ctx, src, &bytes.Buffer{}); err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
