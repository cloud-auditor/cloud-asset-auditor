package policy

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// severityOrder fixes the summary line ordering (most severe first).
var severityOrder = []Severity{SeverityCritical, SeverityError, SeverityWarning, SeverityInfo}

// RenderTable writes a human-readable findings report. totalAssets is the
// number of assets evaluated (the findings alone don't carry it).
func RenderTable(w io.Writer, findings []Finding, totalAssets int) error {
	if len(findings) == 0 {
		_, err := fmt.Fprintf(w, "No findings (%d assets checked).\n", totalAssets)
		return err
	}
	if _, err := fmt.Fprintf(w, "%d finding(s) across %d assets (%s):\n\n", len(findings), totalAssets, summaryLine(findings)); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "SEVERITY\tRULE\tASSET\tPROBLEM")
	for _, f := range findings {
		asset := f.Provider + "/" + f.Type + " " + f.ID
		if f.Name != "" {
			asset += " (" + f.Name + ")"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", f.Severity, f.Rule, asset, f.Problem)
	}
	return tw.Flush()
}

// RenderJSON writes the findings and a summary as one JSON document.
func RenderJSON(w io.Writer, findings []Finding, totalAssets int) error {
	if findings == nil {
		findings = []Finding{}
	}
	doc := struct {
		Findings []Finding      `json:"findings"`
		Summary  map[string]any `json:"summary"`
	}{
		Findings: findings,
		Summary: map[string]any{
			"total_assets": totalAssets,
			"findings":     len(findings),
			"by_severity":  CountBySeverity(findings),
		},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(doc)
}

// RenderMarkdown writes a GitHub-flavored Markdown report, suitable for PR
// comments or $GITHUB_STEP_SUMMARY.
func RenderMarkdown(w io.Writer, findings []Finding, totalAssets int) error {
	if _, err := fmt.Fprintln(w, "## Policy check"); err != nil {
		return err
	}
	if len(findings) == 0 {
		_, err := fmt.Fprintf(w, "\nNo findings (%d assets checked).\n", totalAssets)
		return err
	}
	fmt.Fprintf(w, "\n%d finding(s) across %d assets (%s).\n\n", len(findings), totalAssets, summaryLine(findings))
	fmt.Fprintln(w, "| Severity | Rule | Asset | Problem |")
	fmt.Fprintln(w, "| --- | --- | --- | --- |")
	for _, f := range findings {
		asset := "`" + f.Provider + "/" + f.Type + " " + f.ID + "`"
		if f.Name != "" {
			asset += " (" + escapeMarkdown(f.Name) + ")"
		}
		if _, err := fmt.Fprintf(w, "| %s | %s | %s | %s |\n",
			f.Severity, escapeMarkdown(f.Rule), asset, escapeMarkdown(f.Problem)); err != nil {
			return err
		}
	}
	return nil
}

func summaryLine(findings []Finding) string {
	counts := CountBySeverity(findings)
	line := ""
	for _, sev := range severityOrder {
		if counts[sev] == 0 {
			continue
		}
		if line != "" {
			line += ", "
		}
		line += fmt.Sprintf("%d %s", counts[sev], sev)
	}
	return line
}

// escapeMarkdown neutralizes the table separator; rule names and problems
// are program-generated, so this is the only metacharacter that can break
// the layout.
func escapeMarkdown(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '|' {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}
