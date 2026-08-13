package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// runTopology drives the topology subcommand in isolation — no root command,
// so no config file, logger, or secrets vault gets installed as a side effect
// of asserting on flag handling. Output goes through --output-file because
// openOutput writes to os.Stdout otherwise.
func runTopology(t *testing.T, snapshot string, args ...string) (string, error) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out.txt")

	cmd := newTopologyCmd(&cliState{v: viper.New()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	full := append([]string{"--output-file", out, "--from-snapshot", snapshot}, args...)
	cmd.SetArgs(full)

	err := cmd.Execute()
	b, readErr := os.ReadFile(out) //nolint:gosec // test-owned temp path
	if readErr != nil {
		return "", err
	}
	return string(b), err
}

// orphanSnapshot is a two-provider estate with one of each interesting case: a
// DNS record that resolves to the load balancer, a second that resolves to
// nothing, and a bucket no resolver has ever had an opinion about.
func orphanSnapshot(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "assets.json")
	const body = `[
{"provider":"cloudflare","account_id":"a","type":"cloudflare.dns_record","id":"d1","name":"api.example.com","tags":{"type":"A","content":"10.0.0.5"}},
{"provider":"cloudflare","account_id":"a","type":"cloudflare.dns_record","id":"d2","name":"old.example.com","tags":{"type":"A","content":"203.0.113.9"}},
{"provider":"oci","account_id":"t","region":"eu-frankfurt-1","type":"oci.load_balancer","id":"lb1","name":"prod-lb","tags":{"ip_addresses":"10.0.0.5"}},
{"provider":"oci","account_id":"t","region":"uk-london-1","type":"oci.bucket","id":"b1","name":"backups"}
]`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The default must be the table. The counts are worthless without the prose
// that says what a degree-0 node does not mean, and only the table puts that
// prose in front of a human without being asked.
func TestTopologyOrphans_DefaultsToTheTableSoTheCaveatIsSeen(t *testing.T) {
	got, err := runTopology(t, orphanSnapshot(t), "--orphans")
	if err != nil {
		t.Fatalf("topology --orphans: %v", err)
	}
	if strings.HasPrefix(strings.TrimSpace(got), "{") {
		t.Fatalf("--orphans defaulted to JSON; the caveat must be shown by default:\n%s", got)
	}
	for _, want := range []string{"Orphan report", "safe to delete", "old.example.com", "oci.bucket"} {
		if !strings.Contains(got, want) {
			t.Errorf("report should contain %q:\n%s", want, got)
		}
	}
	// The classification must survive the CLI: a DNS record whose sibling
	// resolved is a lead, a bucket nothing models is noise.
	unconnectedAt := strings.Index(got, "CONNECTABLE, BUT NOT CONNECTED HERE")
	unmodelledAt := strings.Index(got, "NO RESOLVER RELATES THESE TYPES")
	if unconnectedAt < 0 || unmodelledAt < unconnectedAt {
		t.Fatalf("sections missing or out of order:\n%s", got)
	}
	if strings.Index(got, "old.example.com") > unmodelledAt {
		t.Errorf("the unresolved DNS record belongs in the connectable section:\n%s", got)
	}
	if strings.Index(got, "backups") < unmodelledAt {
		t.Errorf("the bucket belongs in the unmodelled section:\n%s", got)
	}
}

func TestTopologyOrphans_HonoursExplicitJSON(t *testing.T) {
	got, err := runTopology(t, orphanSnapshot(t), "--orphans", "-o", "json")
	if err != nil {
		t.Fatalf("topology --orphans -o json: %v", err)
	}
	var report struct {
		Caveat      []string `json:"caveat"`
		Unconnected []struct {
			Type  string `json:"type"`
			Count int    `json:"count"`
		} `json:"unconnected"`
	}
	if err := json.Unmarshal([]byte(got), &report); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, got)
	}
	if len(report.Caveat) < 3 {
		t.Errorf("JSON must carry the caveat, got %v", report.Caveat)
	}
	if len(report.Unconnected) != 1 || report.Unconnected[0].Type != "cloudflare.dns_record" {
		t.Errorf("unexpected classification: %+v", report.Unconnected)
	}
}

// Collapsing drops the edges that stayed inside a group, so a collapsed node
// can read as degree 0 while every asset inside it is connected. Refusing is
// the only honest answer; printing the wrong number is not.
func TestTopologyOrphans_RefusesCollapsedDetailLevels(t *testing.T) {
	for _, detail := range []string{"medium", "high"} {
		_, err := runTopology(t, orphanSnapshot(t), "--orphans", "--detail", detail)
		if err == nil {
			t.Fatalf("--orphans --detail %s must be refused", detail)
		}
		if !strings.Contains(err.Error(), "--detail low") || !strings.Contains(err.Error(), "inside a group") {
			t.Errorf("error must explain why and what to do instead: %v", err)
		}
	}
	if _, err := runTopology(t, orphanSnapshot(t), "--orphans", "--detail", "low"); err != nil {
		t.Errorf("--detail low is the supported combination: %v", err)
	}
}

// --group-by is the report's bucketing dimension, not just a renderer knob.
func TestTopologyOrphans_ComposesWithGroupBy(t *testing.T) {
	got, err := runTopology(t, orphanSnapshot(t), "--orphans", "--group-by", "region")
	if err != nil {
		t.Fatalf("topology --orphans --group-by region: %v", err)
	}
	if !strings.Contains(got, "uk-london-1") {
		t.Errorf("--group-by region must bucket by region:\n%s", got)
	}
}

// Both narrowing flags move the count in a direction a reader would otherwise
// blame on the estate, so both have to say so.
func TestNarrowingCaveats_NameTheFlagThatMovedTheNumber(t *testing.T) {
	if got := narrowingCaveats(nil, nil); len(got) != 0 {
		t.Errorf("an unnarrowed run needs no extra caveat, got %v", got)
	}

	host := strings.Join(narrowingCaveats([]string{"api.example.com"}, nil), " ")
	if !strings.Contains(host, "--hostname") || !strings.Contains(host, "a low count here") {
		t.Errorf("--hostname deflates the count and must say so: %q", host)
	}

	filtered := strings.Join(narrowingCaveats(nil, []string{"provider=oci"}), " ")
	if !strings.Contains(filtered, "--filter") || !strings.Contains(filtered, "counted here as an orphan") {
		t.Errorf("--filter inflates the count and must say so: %q", filtered)
	}
}

func TestTopologyOrphans_HostnameCaveatReachesTheReport(t *testing.T) {
	got, err := runTopology(t, orphanSnapshot(t), "--orphans", "--hostname", "api.example.com")
	if err != nil {
		t.Fatalf("topology --orphans --hostname: %v", err)
	}
	if !strings.Contains(got, "--hostname narrowed the graph") {
		t.Errorf("the hostname caveat must reach the rendered report:\n%s", got)
	}
}

// A graph format has no meaning for a report about the nodes that have no
// edges. Rejecting beats emitting a page of loose boxes.
func TestTopologyOrphans_RejectsGraphFormats(t *testing.T) {
	_, err := runTopology(t, orphanSnapshot(t), "--orphans", "-o", "dot")
	if err == nil {
		t.Fatal("--orphans -o dot must be refused")
	}
	if !strings.Contains(err.Error(), "table|json") {
		t.Errorf("error should name the two formats that work: %v", err)
	}
}

// Without --orphans nothing changes: the same flags still render a diagram.
func TestTopology_WithoutOrphansStillRendersTheGraph(t *testing.T) {
	got, err := runTopology(t, orphanSnapshot(t), "-o", "dot")
	if err != nil {
		t.Fatalf("topology -o dot: %v", err)
	}
	if !strings.Contains(got, "digraph") {
		t.Errorf("the diagram path must be untouched:\n%s", got)
	}
}

// Gating a pipeline on this number would institutionalise exactly the reading
// the report spends its first paragraph disowning. If someone adds --exit-code
// here, this test is where the argument has to be had.
func TestTopologyOrphans_HasNoExitCodeGate(t *testing.T) {
	cmd := newTopologyCmd(&cliState{v: viper.New()})
	if cmd.Flags().Lookup("orphans") == nil {
		t.Fatal("--orphans is not registered")
	}
	if cmd.Flags().Lookup("exit-code") != nil {
		t.Error("topology registered --exit-code; an orphan is a question, not a finding to fail a build on")
	}
}
