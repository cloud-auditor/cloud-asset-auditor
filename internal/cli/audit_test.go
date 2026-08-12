package cli

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

type fakeProvider struct{ name string }

func (f fakeProvider) Name() string                   { return f.name }
func (f fakeProvider) Validate(context.Context) error { return nil }
func (f fakeProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	return nil, nil
}

// The audit cache keys on the providers that ACTUALLY run, not the raw
// request. The "none" sentinel resolves to zero providers — so its key set is
// empty and the audit command skips the cache entirely (it can't be confused
// for a real provider's snapshot).
func TestSelectProviders_NoneSentinelYieldsNoProviders(t *testing.T) {
	if got := selectProviders([]string{"none"}); len(got) != 0 {
		t.Errorf("selectProviders([none]) = %d providers, want 0", len(got))
	}
}

func TestProviderNames_DerivesKeyFromRealizedProviders(t *testing.T) {
	got := providerNames([]core.Provider{fakeProvider{"oci"}, fakeProvider{"netbird"}})
	if len(got) != 2 || got[0] != "oci" || got[1] != "netbird" {
		t.Errorf("providerNames = %v, want [oci netbird]", got)
	}
	if providerNames(nil) == nil {
		// returns an empty, non-nil slice — fine either way, just shouldn't panic
		t.Log("providerNames(nil) returned nil (acceptable)")
	}
}

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
