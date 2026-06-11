package diff

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// The renderers live here (not in internal/cli) so they can be unit-tested
// against an io.Writer alongside Compute — the cli package has no command
// test harness today. Each takes the snapshot totals separately because
// Result intentionally carries only the drift, not the inputs.

// summary mirrors the "summary" object of the JSON report. Defined at
// package level (not inline in RenderJSON) only so the JSON shape is
// greppable next to Result.
type summary struct {
	Added    int `json:"added"`
	Removed  int `json:"removed"`
	Changed  int `json:"changed"`
	OldTotal int `json:"old_total"`
	NewTotal int `json:"new_total"`
}

// RenderJSON writes the machine-readable report:
// {added, removed, changed, summary:{added,removed,changed,old_total,new_total}}.
// Compact single-line output, matching the audit JSON renderer's style.
func RenderJSON(w io.Writer, res Result, oldTotal, newTotal int) error {
	report := struct {
		Added   []core.Asset `json:"added"`
		Removed []core.Asset `json:"removed"`
		Changed []Change     `json:"changed"`
		Summary summary      `json:"summary"`
	}{
		Added:   res.Added,
		Removed: res.Removed,
		Changed: res.Changed,
		Summary: summary{
			Added:    len(res.Added),
			Removed:  len(res.Removed),
			Changed:  len(res.Changed),
			OldTotal: oldTotal,
			NewTotal: newTotal,
		},
	}
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("encode diff report: %w", err)
	}
	return nil
}

// RenderTable writes the human-readable summary: a counts header, then one
// section per category. Values in field changes are %q-quoted so an empty
// old/new value is visible rather than vanishing into whitespace.
func RenderTable(w io.Writer, res Result, oldTotal, newTotal int) error {
	bw := bufio.NewWriter(w)

	if res.Empty() {
		fmt.Fprintf(bw, "No drift (old: %d assets, new: %d assets).\n", oldTotal, newTotal)
		return bw.Flush()
	}

	fmt.Fprintf(bw, "%d added, %d removed, %d changed (old: %d assets, new: %d assets)\n",
		len(res.Added), len(res.Removed), len(res.Changed), oldTotal, newTotal)

	if len(res.Added) > 0 {
		fmt.Fprintf(bw, "\nAdded:\n")
		for _, a := range res.Added {
			fmt.Fprintf(bw, "  + %s\n", assetLine(a))
		}
	}
	if len(res.Removed) > 0 {
		fmt.Fprintf(bw, "\nRemoved:\n")
		for _, a := range res.Removed {
			fmt.Fprintf(bw, "  - %s\n", assetLine(a))
		}
	}
	if len(res.Changed) > 0 {
		fmt.Fprintf(bw, "\nChanged:\n")
		for _, c := range res.Changed {
			fmt.Fprintf(bw, "  ~ %s\n", assetLine(c.After))
			for _, f := range c.Fields {
				fmt.Fprintf(bw, "      %s: %q -> %q\n", f.Field, f.Old, f.New)
			}
		}
	}
	return bw.Flush()
}

// RenderMarkdown writes the same content as RenderTable in GitHub-flavored
// markdown — sized for pasting into a PR comment from a CI gate.
func RenderMarkdown(w io.Writer, res Result, oldTotal, newTotal int) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "## Audit drift\n\n")
	if res.Empty() {
		fmt.Fprintf(bw, "No drift (old: %d assets, new: %d assets).\n", oldTotal, newTotal)
		return bw.Flush()
	}

	fmt.Fprintf(bw, "**%d added, %d removed, %d changed** (old: %d assets, new: %d assets)\n",
		len(res.Added), len(res.Removed), len(res.Changed), oldTotal, newTotal)

	if len(res.Added) > 0 {
		fmt.Fprintf(bw, "\n### Added (%d)\n\n", len(res.Added))
		for _, a := range res.Added {
			fmt.Fprintf(bw, "- %s\n", assetLineMarkdown(a))
		}
	}
	if len(res.Removed) > 0 {
		fmt.Fprintf(bw, "\n### Removed (%d)\n\n", len(res.Removed))
		for _, a := range res.Removed {
			fmt.Fprintf(bw, "- %s\n", assetLineMarkdown(a))
		}
	}
	if len(res.Changed) > 0 {
		fmt.Fprintf(bw, "\n### Changed (%d)\n\n", len(res.Changed))
		for _, c := range res.Changed {
			fmt.Fprintf(bw, "- %s\n", assetLineMarkdown(c.After))
			for _, f := range c.Fields {
				fmt.Fprintf(bw, "  - `%s`: `%q` → `%q`\n", f.Field, f.Old, f.New)
			}
		}
	}
	return bw.Flush()
}

// assetLine is the one-line identity used in table sections:
// provider/type id (name). Name falls back to the ID so unnamed assets
// don't render a dangling "()".
func assetLine(a core.Asset) string {
	return fmt.Sprintf("%s/%s %s (%s)", a.Provider, a.Type, a.ID, displayName(a))
}

func assetLineMarkdown(a core.Asset) string {
	return fmt.Sprintf("`%s/%s` `%s` — %s", a.Provider, a.Type, a.ID, displayName(a))
}

func displayName(a core.Asset) string {
	if a.Name != "" {
		return a.Name
	}
	return a.ID
}
