package cli

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

func TestBuildRenderer_SelectsFormat(t *testing.T) {
	tests := []struct {
		format string
		want   string
	}{
		{"json", "*output.JSON"},
		{"csv", "*output.CSV"},
		{"xlsx", "*output.XLSX"},
		{"html", "*output.HTML"},
		{"HTML", "*output.HTML"}, // format matching is case-insensitive
	}
	for _, tc := range tests {
		// sheetBy/summary ride along with their flag defaults: xlsx consumes
		// them, every other format ignores them.
		r, err := buildRenderer(tc.format, false, "provider", true)
		if err != nil {
			t.Errorf("buildRenderer(%q) error: %v", tc.format, err)
			continue
		}
		if got := fmt.Sprintf("%T", r); got != tc.want {
			t.Errorf("buildRenderer(%q) = %s, want %s", tc.format, got, tc.want)
		}
	}
}

func TestBuildRenderer_StreamOnlyWithJSON(t *testing.T) {
	r, err := buildRenderer("json", true, "provider", false)
	if err != nil {
		t.Fatalf("buildRenderer(json, stream): %v", err)
	}
	if j, ok := r.(*output.JSON); !ok || !j.Stream {
		t.Errorf("buildRenderer(json, stream) = %#v, want *output.JSON with Stream set", r)
	}

	for _, format := range []string{"csv", "xlsx", "html"} {
		if _, err := buildRenderer(format, true, "provider", false); err == nil {
			t.Errorf("buildRenderer(%q, stream) accepted --stream, want error", format)
		}
	}
}

func TestBuildRenderer_InvalidSheetBy(t *testing.T) {
	if _, err := buildRenderer("xlsx", false, "bogus", false); err == nil {
		t.Error("buildRenderer(xlsx, sheet-by=bogus) = nil error, want validation failure")
	}
}

func TestBuildRenderer_UnknownFormatListsSupported(t *testing.T) {
	_, err := buildRenderer("pdf", false, "provider", false)
	if err == nil {
		t.Fatal("buildRenderer(pdf) = nil error, want unknown-format error")
	}
	for _, want := range []string{"json", "csv", "xlsx", "html"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not list supported format %q", err, want)
		}
	}
}
