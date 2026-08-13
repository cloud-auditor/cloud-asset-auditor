package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/insight"
)

// runInsights drives the subcommand in isolation — no root command, so no
// config file, logger, or secrets vault is installed as a side effect of
// asserting on flag handling. Same harness as runTopology, and for the same
// reason: output goes through --output-file because openOutput writes to
// os.Stdout otherwise.
func runInsights(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.txt")

	cmd := newInsightsCmd(&cliState{v: viper.New()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	cmd.SetArgs(append([]string{"--output-file", out}, args...))

	err := cmd.Execute()
	b, readErr := os.ReadFile(out) //nolint:gosec // test-owned temp path
	if readErr != nil {
		return "", err
	}
	return string(b), err
}

// insightSnapshot is a small cross-provider estate: a DNS record resolving to
// a load balancer that fronts a Service, plus an untagged bucket. It yields
// real edges and at least one finding in more than one family.
func insightSnapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assets.json")
	const body = `[
{"provider":"cloudflare","account_id":"a","type":"cloudflare.dns_record","id":"d1","name":"api.example.com","tags":{"type":"A","content":"203.0.113.10"}},
{"provider":"oci","account_id":"t","region":"eu-frankfurt-1","type":"oci.load_balancer","id":"lb1","name":"prod-lb","tags":{"ip_addresses":"203.0.113.10"}},
{"provider":"kubernetes","account_id":"c1","type":"v1.Service","id":"svc1","name":"api","tags":{"namespace":"shop"},"raw":{"spec":{"selector":{"app":"api"}},"status":{"loadBalancer":{"ingress":[{"ip":"203.0.113.10"}]}}}},
{"provider":"kubernetes","account_id":"c1","type":"v1.Pod","id":"pod1","name":"api-abc","tags":{"namespace":"shop","app":"api"}},
{"provider":"oci","account_id":"t","region":"uk-london-1","type":"oci.bucket","id":"b1","name":"backups"}
]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The table is the default because it is the only format that puts the
// disclaimer and each finding's caveat in front of a human without being asked
// for them.
func TestInsights_TableCarriesTheDisclaimerAndEveryCaveat(t *testing.T) {
	got, err := runInsights(t, "--from-snapshot", insightSnapshot(t))
	if err != nil {
		t.Fatalf("insights: %v", err)
	}
	for _, want := range []string{"An inventory cannot see", "basis", "cannot know"} {
		if !strings.Contains(got, want) {
			t.Errorf("report should contain %q:\n%s", want, got)
		}
	}
}

func TestInsights_JSONPublishesNothingWithoutABasisAndCaveat(t *testing.T) {
	got, err := runInsights(t, "--from-snapshot", insightSnapshot(t), "-o", "json")
	if err != nil {
		t.Fatalf("insights -o json: %v", err)
	}

	var rep insight.Report
	if err := json.Unmarshal([]byte(got), &rep); err != nil {
		t.Fatalf("decode report: %v\n%s", err, got)
	}
	if rep.Disclaimer == "" {
		t.Error("report has no disclaimer")
	}
	for _, f := range rep.Findings {
		if strings.TrimSpace(f.Caveat) == "" || strings.TrimSpace(f.Basis) == "" {
			t.Errorf("finding %q published without a basis and caveat", f.ID)
		}
	}
	if len(rep.Suppressed) > 0 {
		t.Errorf("REFUSED list is non-empty (%v) — that is a bug in an insight", rep.Suppressed)
	}
}

// A typo in -o or --severity must cost a second, not a ten-minute audit.
func TestInsights_ValidatesFlagsBeforeAuditing(t *testing.T) {
	for _, tc := range []struct{ name, flag, value string }{
		{"format", "-o", "dot"},
		{"severity", "--severity", "bogus"},
		{"fail-on", "--fail-on", "catastrophic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// No --from-snapshot: reaching the audit would be the failure.
			if _, err := runInsights(t, tc.flag, tc.value); err == nil {
				t.Fatalf("%s %s was accepted", tc.flag, tc.value)
			}
		})
	}
}

// --list answers a question about the binary, not about an estate, so it must
// not need a snapshot or a credential.
func TestInsights_ListNeedsNoInventory(t *testing.T) {
	got, err := runInsights(t, "--list")
	if err != nil {
		t.Fatalf("insights --list: %v", err)
	}
	if !strings.Contains(got, "registered") {
		t.Fatalf("--list printed no catalogue:\n%s", got)
	}
	// The listing must say what an insight needs, in the same words the NOT RUN
	// section uses — "nothing found" and "never looked" are different answers.
	if !strings.Contains(got, "needs cost estimates (--cost)") {
		t.Errorf("--list does not say what the cost insights need:\n%s", got)
	}
	for _, i := range insight.Registered() {
		if !strings.Contains(got, i.ID()) {
			t.Errorf("--list omitted %q", i.ID())
		}
	}
}

func TestInsights_OnlyFiltersByFamilyAndID(t *testing.T) {
	snapshot := insightSnapshot(t)
	for _, selector := range []string{"network", "network.*"} {
		got, err := runInsights(t, "--from-snapshot", snapshot, "--only", selector, "-o", "json")
		if err != nil {
			t.Fatalf("--only %s: %v", selector, err)
		}
		var rep insight.Report
		if err := json.Unmarshal([]byte(got), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, f := range rep.Findings {
			if f.Family != insight.FamilyNetwork {
				t.Errorf("--only %s leaked a %q finding", selector, f.Family)
			}
		}
	}
}

// --exit-code defaults to gating on risk: the other three severities are
// explicitly questions, and a build that fails on a question teaches the team
// to stop reading the caveats.
func TestInsights_ExitCodeGatesOnRiskByDefault(t *testing.T) {
	snapshot := insightSnapshot(t)

	if _, err := runInsights(t, "--from-snapshot", snapshot, "--exit-code", "-o", "json"); err != nil {
		t.Fatalf("this fixture has no risk-severity finding, so the default gate must pass: %v", err)
	}
	_, err := runInsights(t, "--from-snapshot", snapshot, "--exit-code", "--fail-on", "info", "-o", "json")
	if !errors.Is(err, ErrInsights) {
		t.Fatalf("--fail-on info should trip on any finding, got %v", err)
	}
}

// Cost-bearing insights must be visibly absent, not silently absent.
func TestInsights_CostFamilyIsSkippedWithoutTheFlag(t *testing.T) {
	got, err := runInsights(t, "--from-snapshot", insightSnapshot(t), "--only", "cost", "-o", "json")
	if err != nil {
		t.Fatalf("insights --only cost: %v", err)
	}
	var rep insight.Report
	if err := json.Unmarshal([]byte(got), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Findings) != 0 {
		t.Fatalf("got %d cost findings without --cost", len(rep.Findings))
	}
	if len(rep.Skipped) == 0 {
		t.Fatal("the cost insights vanished instead of being reported as skipped")
	}
}

func TestInsightNeeds_NamesTheFixNotTheField(t *testing.T) {
	for _, i := range insight.Registered() {
		r, ok := i.(insight.Requiring)
		if !ok {
			continue
		}
		if r.Requires().Cost && !strings.Contains(insightNeeds(i), "--cost") {
			t.Errorf("%s needs cost but the listing does not name the flag", i.ID())
		}
	}
}

func TestSampleList_CountsTheTail(t *testing.T) {
	if got := sampleList([]string{"a", "b"}, 3); got != "a, b" {
		t.Errorf("short list = %q", got)
	}
	if got := sampleList([]string{"a", "b", "c", "d", "e"}, 3); got != "a, b, c and 2 more" {
		t.Errorf("long list = %q", got)
	}
}
