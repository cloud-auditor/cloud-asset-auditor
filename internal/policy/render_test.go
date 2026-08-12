package policy

import (
	"encoding/json"
	"strings"
	"testing"
)

func fixtureFindings() []Finding {
	return []Finding{
		{Rule: "require-owner", Severity: SeverityError, Problem: `missing required tag "owner"`,
			Provider: "oci", Type: "oci.instance", ID: "i2", Name: "web-02"},
		{Rule: "no-page-rules", Severity: SeverityCritical, Problem: "asset matches a forbidden pattern",
			Provider: "cloudflare", Type: "cloudflare.page_rule", ID: "pr1", Name: "legacy|pipe"},
	}
}

func TestRenderTable(t *testing.T) {
	var sb strings.Builder
	if err := RenderTable(&sb, fixtureFindings(), 10); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"2 finding(s) across 10 assets",
		"1 critical, 1 error",
		"SEVERITY", "require-owner", "oci/oci.instance i2 (web-02)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTable_Empty(t *testing.T) {
	var sb strings.Builder
	if err := RenderTable(&sb, nil, 5); err != nil {
		t.Fatal(err)
	}
	if got := sb.String(); got != "No findings (5 assets checked).\n" {
		t.Errorf("empty table = %q", got)
	}
}

func TestRenderJSON(t *testing.T) {
	var sb strings.Builder
	if err := RenderJSON(&sb, fixtureFindings(), 10); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Findings []Finding `json:"findings"`
		Summary  struct {
			TotalAssets int              `json:"total_assets"`
			Findings    int              `json:"findings"`
			BySeverity  map[Severity]int `json:"by_severity"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(sb.String()), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, sb.String())
	}
	if len(doc.Findings) != 2 || doc.Summary.TotalAssets != 10 || doc.Summary.Findings != 2 {
		t.Errorf("unexpected doc: %+v", doc)
	}
	if doc.Summary.BySeverity[SeverityCritical] != 1 {
		t.Errorf("by_severity = %v", doc.Summary.BySeverity)
	}
}

func TestRenderJSON_EmptyIsArrayNotNull(t *testing.T) {
	var sb strings.Builder
	if err := RenderJSON(&sb, nil, 0); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), `"findings":[]`) {
		t.Errorf("nil findings must serialize as []: %s", sb.String())
	}
}

func TestRenderMarkdown(t *testing.T) {
	var sb strings.Builder
	if err := RenderMarkdown(&sb, fixtureFindings(), 10); err != nil {
		t.Fatal(err)
	}
	out := sb.String()
	for _, want := range []string{
		"## Policy check",
		"| Severity | Rule | Asset | Problem |",
		"`oci/oci.instance i2`",
		`legacy\|pipe`, // pipe in a name must be escaped so the table survives
	} {
		if !strings.Contains(out, want) {
			t.Errorf("markdown missing %q:\n%s", want, out)
		}
	}
}

func TestRenderMarkdown_Empty(t *testing.T) {
	var sb strings.Builder
	if err := RenderMarkdown(&sb, nil, 3); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "No findings (3 assets checked).") {
		t.Errorf("empty markdown = %q", sb.String())
	}
}
