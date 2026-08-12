package policy

import (
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func mustLoad(t *testing.T, doc string) []Rule {
	t.Helper()
	rules, err := Load(strings.NewReader(doc))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return rules
}

func TestLoad_Valid(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: r1
    description: first
    severity: error
    match:
      providers: [oci]
    assert:
      required_tags: [owner]
  - name: r2
    match:
      types: ["cloudflare.page_rule"]
`)
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	if rules[0].Severity != SeverityError {
		t.Errorf("r1 severity = %q, want error", rules[0].Severity)
	}
	if rules[1].Severity != SeverityWarning {
		t.Errorf("r2 severity = %q, want defaulted warning", rules[1].Severity)
	}
	if rules[1].Assert != nil {
		t.Error("r2 assert should be nil (match-only rule)")
	}
}

func TestLoad_Errors(t *testing.T) {
	cases := map[string]string{
		"empty doc":      ``,
		"no rules key":   `foo: bar`,
		"missing name":   "rules:\n  - severity: error",
		"duplicate name": "rules:\n  - name: a\n  - name: a",
		"bad severity":   "rules:\n  - name: a\n    severity: fatal",
		"empty assert":   "rules:\n  - name: a\n    assert: {}",
		"not yaml":       `{{{{`,
	}
	for label, doc := range cases {
		if _, err := Load(strings.NewReader(doc)); err == nil {
			t.Errorf("%s: want error, got nil", label)
		}
	}
}

func TestLoad_ScalarAndListPatterns(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: scalar
    match:
      tags:
        env: prod
    assert:
      tag_matches:
        tier: [web, api]
`)
	if got := rules[0].Match.Tags["env"]; len(got) != 1 || got[0] != "prod" {
		t.Errorf("scalar tag pattern = %v, want [prod]", got)
	}
	if got := rules[0].Assert.TagMatches["tier"]; len(got) != 2 {
		t.Errorf("list tag pattern = %v, want two entries", got)
	}
}

func TestStarterRules_Load(t *testing.T) {
	rules, err := Load(strings.NewReader(StarterRules))
	if err != nil {
		t.Fatalf("StarterRules must load cleanly: %v", err)
	}
	if len(rules) == 0 {
		t.Fatal("StarterRules loaded zero rules")
	}
}

func testAssets() []core.Asset {
	return []core.Asset{
		{Provider: "oci", Type: "oci.instance", ID: "i1", Name: "web-01",
			Status: "RUNNING", Tags: map[string]string{"owner": "team-a", "env": "prod"}},
		{Provider: "oci", Type: "oci.instance", ID: "i2", Name: "web-02",
			Status: "STOPPED", Tags: map[string]string{"env": "dev"}},
		{Provider: "cloudflare", Type: "cloudflare.page_rule", ID: "pr1", Name: "legacy"},
	}
}

func TestEvaluate_RequiredTags(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: require-owner
    severity: error
    match:
      providers: [oci]
    assert:
      required_tags: [owner]
`)
	findings := Evaluate(rules, testAssets())
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	f := findings[0]
	if f.ID != "i2" || f.Severity != SeverityError || !strings.Contains(f.Problem, `missing required tag "owner"`) {
		t.Errorf("unexpected finding: %+v", f)
	}
}

func TestEvaluate_MatchOnlyRuleFlagsEverything(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: no-page-rules
    severity: critical
    match:
      types: ["cloudflare.page_rule"]
`)
	findings := Evaluate(rules, testAssets())
	if len(findings) != 1 || findings[0].ID != "pr1" {
		t.Fatalf("got %+v, want one finding on pr1", findings)
	}
	if findings[0].Problem != "asset matches a forbidden pattern" {
		t.Errorf("problem = %q", findings[0].Problem)
	}
}

func TestEvaluate_StatusAndCombinedProblems(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: healthy-instances
    match:
      types: ["oci.instance"]
    assert:
      required_tags: [owner]
      status_in: [running]
`)
	findings := Evaluate(rules, testAssets())
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1 (i2 fails both, i1 passes both)", len(findings))
	}
	p := findings[0].Problem
	if !strings.Contains(p, "missing required tag") || !strings.Contains(p, `status "STOPPED"`) {
		t.Errorf("combined problems missing: %q", p)
	}
}

func TestEvaluate_ForbiddenTagsAndTagMatches(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: tags
    assert:
      forbidden_tags: [deprecated]
      tag_matches:
        env: [prod, staging]
`)
	assets := []core.Asset{
		{Provider: "p", Type: "t", ID: "ok", Tags: map[string]string{"env": "prod"}},
		{Provider: "p", Type: "t", ID: "bad-env", Tags: map[string]string{"env": "dev"}},
		{Provider: "p", Type: "t", ID: "deprecated", Tags: map[string]string{"env": "prod", "deprecated": "1"}},
	}
	findings := Evaluate(rules, assets)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}
	byID := map[string]Finding{}
	for _, f := range findings {
		byID[f.ID] = f
	}
	if !strings.Contains(byID["bad-env"].Problem, `does not match`) {
		t.Errorf("bad-env problem = %q", byID["bad-env"].Problem)
	}
	if !strings.Contains(byID["deprecated"].Problem, `forbidden tag`) {
		t.Errorf("deprecated problem = %q", byID["deprecated"].Problem)
	}
}

func TestEvaluate_MatchNarrowing(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: narrow
    match:
      providers: [oci]
      statuses: [stopped]
      tags:
        env: dev
    assert:
      required_tags: [owner]
`)
	findings := Evaluate(rules, testAssets())
	if len(findings) != 1 || findings[0].ID != "i2" {
		t.Fatalf("match narrowing failed: %+v", findings)
	}
}

func TestEvaluate_DeterministicOrder(t *testing.T) {
	rules := mustLoad(t, `
rules:
  - name: all
    assert:
      required_tags: [nonexistent]
`)
	assets := testAssets()
	a := Evaluate(rules, assets)
	// Reverse the input; output order must not change.
	reversed := []core.Asset{assets[2], assets[1], assets[0]}
	b := Evaluate(rules, reversed)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("finding %d differs: %+v vs %+v", i, a[i], b[i])
		}
	}
}

func TestSeverityHelpers(t *testing.T) {
	if _, err := ParseSeverity("warn"); err != nil {
		t.Error("warn alias should parse")
	}
	if _, err := ParseSeverity("fatal"); err == nil {
		t.Error("fatal should not parse")
	}
	if SeverityCritical.Rank() <= SeverityError.Rank() {
		t.Error("critical must outrank error")
	}

	findings := []Finding{{Severity: SeverityInfo}, {Severity: SeverityError}, {Severity: SeverityWarning}}
	max, ok := MaxSeverity(findings)
	if !ok || max != SeverityError {
		t.Errorf("MaxSeverity = %v, %v", max, ok)
	}
	if _, ok := MaxSeverity(nil); ok {
		t.Error("MaxSeverity(nil) must report ok=false")
	}
	counts := CountBySeverity(findings)
	if counts[SeverityInfo] != 1 || counts[SeverityError] != 1 || counts[SeverityWarning] != 1 {
		t.Errorf("CountBySeverity = %v", counts)
	}
}
