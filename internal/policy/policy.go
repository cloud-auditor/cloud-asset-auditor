// Package policy implements the rules engine behind `auditor check`: YAML
// rule files are loaded into Rules, evaluated against a set of assets, and
// the resulting Findings rendered as a table, JSON, or Markdown.
//
// A rule selects assets with a Match block (glob lists that OR within a
// field and AND across fields) and then either flags every selected asset
// (no assert block) or checks the assertions in its Assert block, emitting
// one finding per asset that fails. All globs are case-insensitive with *
// as the only wildcard (see internal/filter.Glob).
package policy

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
)

// Severity classifies a finding. The zero value is invalid; Load defaults a
// rule's empty severity to SeverityWarning.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

var severityRank = map[Severity]int{
	SeverityInfo:     0,
	SeverityWarning:  1,
	SeverityError:    2,
	SeverityCritical: 3,
}

// ParseSeverity normalizes a user-supplied severity name. "warn" is accepted
// as an alias for "warning".
func ParseSeverity(s string) (Severity, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "info":
		return SeverityInfo, nil
	case "warn", "warning":
		return SeverityWarning, nil
	case "error":
		return SeverityError, nil
	case "critical":
		return SeverityCritical, nil
	default:
		return "", fmt.Errorf("unknown severity %q (want info|warning|error|critical)", s)
	}
}

// Rank orders severities: info < warning < error < critical.
func (s Severity) Rank() int { return severityRank[s] }

// Rule is one policy rule. Match narrows the assets the rule applies to;
// Assert lists the conditions those assets must satisfy. A rule with no
// assert block flags every matched asset — use that shape to forbid whole
// classes of assets (e.g. match a type that should not exist).
type Rule struct {
	Name        string   `yaml:"name" json:"name"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Severity    Severity `yaml:"severity,omitempty" json:"severity,omitempty"`
	Match       Match    `yaml:"match,omitempty" json:"match,omitempty"`
	Assert      *Assert  `yaml:"assert,omitempty" json:"assert,omitempty"`
}

// PatternList is a list of glob alternatives that unmarshals from either a
// single YAML scalar ("prod") or a sequence (["prod", "staging"]).
type PatternList []string

// UnmarshalYAML accepts a scalar as a one-element list.
func (p *PatternList) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		*p = PatternList{s}
		return nil
	}
	var list []string
	if err := node.Decode(&list); err != nil {
		return err
	}
	*p = PatternList(list)
	return nil
}

// Match selects assets. Within one field the globs OR together; across
// fields the conditions AND. Tags requires each listed tag to exist and
// glob-match one of its patterns. An entirely empty Match selects every
// asset.
type Match struct {
	Providers []string               `yaml:"providers,omitempty" json:"providers,omitempty"`
	Types     []string               `yaml:"types,omitempty" json:"types,omitempty"`
	Regions   []string               `yaml:"regions,omitempty" json:"regions,omitempty"`
	Accounts  []string               `yaml:"accounts,omitempty" json:"accounts,omitempty"`
	Statuses  []string               `yaml:"statuses,omitempty" json:"statuses,omitempty"`
	Names     []string               `yaml:"names,omitempty" json:"names,omitempty"`
	Tags      map[string]PatternList `yaml:"tags,omitempty" json:"tags,omitempty"`
}

// Assert lists conditions a matched asset must satisfy. Every non-empty
// assertion is checked; each failure contributes to the asset's finding.
type Assert struct {
	// RequiredTags must all be present (any value, including empty).
	RequiredTags []string `yaml:"required_tags,omitempty" json:"required_tags,omitempty"`
	// ForbiddenTags must all be absent.
	ForbiddenTags []string `yaml:"forbidden_tags,omitempty" json:"forbidden_tags,omitempty"`
	// TagMatches requires each tag to be present AND glob-match one of its
	// patterns (scalar or list in YAML).
	TagMatches map[string]PatternList `yaml:"tag_matches,omitempty" json:"tag_matches,omitempty"`
	// StatusIn requires the asset status to glob-match one of the patterns.
	StatusIn []string `yaml:"status_in,omitempty" json:"status_in,omitempty"`
	// NameMatches requires the asset name to glob-match one of the patterns.
	NameMatches []string `yaml:"name_matches,omitempty" json:"name_matches,omitempty"`
}

func (a *Assert) empty() bool {
	return len(a.RequiredTags) == 0 && len(a.ForbiddenTags) == 0 &&
		len(a.TagMatches) == 0 && len(a.StatusIn) == 0 && len(a.NameMatches) == 0
}

type document struct {
	Rules []Rule `yaml:"rules"`
}

// Load parses a YAML rules document ({rules: [...]}) and validates it: rule
// names must be present and unique, severities must parse (empty defaults to
// warning), and an assert block, when present, must assert something.
func Load(r io.Reader) ([]Rule, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read rules: %w", err)
	}
	var doc document
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse rules: %w", err)
	}
	if len(doc.Rules) == 0 {
		return nil, fmt.Errorf("no rules found (want a top-level \"rules:\" list)")
	}
	seen := make(map[string]bool, len(doc.Rules))
	for i := range doc.Rules {
		rule := &doc.Rules[i]
		if rule.Name == "" {
			return nil, fmt.Errorf("rule %d: missing name", i+1)
		}
		if seen[rule.Name] {
			return nil, fmt.Errorf("rule %q: duplicate name", rule.Name)
		}
		seen[rule.Name] = true
		if rule.Severity == "" {
			rule.Severity = SeverityWarning
		} else {
			sev, err := ParseSeverity(string(rule.Severity))
			if err != nil {
				return nil, fmt.Errorf("rule %q: %w", rule.Name, err)
			}
			rule.Severity = sev
		}
		if rule.Assert != nil && rule.Assert.empty() {
			return nil, fmt.Errorf("rule %q: assert block is empty (remove it to flag every matched asset)", rule.Name)
		}
	}
	return doc.Rules, nil
}

// Finding is one rule violation on one asset.
type Finding struct {
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	Description string   `json:"description,omitempty"`
	Problem     string   `json:"problem"`
	Provider    string   `json:"provider"`
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Name        string   `json:"name,omitempty"`
}

// Evaluate runs every rule over every asset. Output order is deterministic:
// rules in file order, assets sorted by (provider, type, id) within a rule.
func Evaluate(rules []Rule, assets []core.Asset) []Finding {
	sorted := append([]core.Asset(nil), assets...)
	sort.Slice(sorted, func(i, j int) bool {
		a, b := sorted[i], sorted[j]
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		return a.ID < b.ID
	})

	var findings []Finding
	for _, rule := range rules {
		for _, a := range sorted {
			if !rule.Match.matches(a) {
				continue
			}
			problems := rule.problems(a)
			if len(problems) == 0 {
				continue
			}
			findings = append(findings, Finding{
				Rule:        rule.Name,
				Severity:    rule.Severity,
				Description: rule.Description,
				Problem:     strings.Join(problems, "; "),
				Provider:    a.Provider,
				Type:        a.Type,
				ID:          a.ID,
				Name:        a.Name,
			})
		}
	}
	return findings
}

func (m Match) matches(a core.Asset) bool {
	if !globAny(m.Providers, a.Provider) ||
		!globAny(m.Types, a.Type) ||
		!globAny(m.Regions, a.Region) ||
		!globAny(m.Accounts, a.AccountID) ||
		!globAny(m.Statuses, a.Status) ||
		!globAny(m.Names, a.Name) {
		return false
	}
	for key, patterns := range m.Tags {
		value, ok := a.Tags[key]
		if !ok || !globAny(patterns, value) {
			return false
		}
	}
	return true
}

// problems returns the human-readable assertion failures for a matched
// asset, in a deterministic order. A nil Assert flags the asset itself.
func (r Rule) problems(a core.Asset) []string {
	if r.Assert == nil {
		return []string{"asset matches a forbidden pattern"}
	}
	var problems []string
	for _, tag := range r.Assert.RequiredTags {
		if _, ok := a.Tags[tag]; !ok {
			problems = append(problems, fmt.Sprintf("missing required tag %q", tag))
		}
	}
	for _, tag := range r.Assert.ForbiddenTags {
		if _, ok := a.Tags[tag]; ok {
			problems = append(problems, fmt.Sprintf("carries forbidden tag %q", tag))
		}
	}
	for _, key := range sortedKeys(r.Assert.TagMatches) {
		patterns := r.Assert.TagMatches[key]
		value, ok := a.Tags[key]
		switch {
		case !ok:
			problems = append(problems, fmt.Sprintf("missing required tag %q", key))
		case !globAny(patterns, value):
			problems = append(problems, fmt.Sprintf("tag %q value %q does not match %v", key, value, []string(patterns)))
		}
	}
	if len(r.Assert.StatusIn) > 0 && !globAny(r.Assert.StatusIn, a.Status) {
		problems = append(problems, fmt.Sprintf("status %q not in %v", a.Status, r.Assert.StatusIn))
	}
	if len(r.Assert.NameMatches) > 0 && !globAny(r.Assert.NameMatches, a.Name) {
		problems = append(problems, fmt.Sprintf("name %q does not match %v", a.Name, r.Assert.NameMatches))
	}
	return problems
}

// globAny reports whether value matches any pattern; an empty pattern list
// matches everything (the field is unconstrained).
func globAny(patterns []string, value string) bool {
	if len(patterns) == 0 {
		return true
	}
	for _, p := range patterns {
		if filter.Glob(p, value) {
			return true
		}
	}
	return false
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// MaxSeverity returns the highest severity among findings, and false when
// there are none.
func MaxSeverity(findings []Finding) (Severity, bool) {
	if len(findings) == 0 {
		return "", false
	}
	max := findings[0].Severity
	for _, f := range findings[1:] {
		if f.Severity.Rank() > max.Rank() {
			max = f.Severity
		}
	}
	return max, true
}

// CountBySeverity tallies findings per severity.
func CountBySeverity(findings []Finding) map[Severity]int {
	counts := make(map[Severity]int)
	for _, f := range findings {
		counts[f.Severity]++
	}
	return counts
}
