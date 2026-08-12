package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// ----------------------------------------------------------------------
// provider option plumbing
// ----------------------------------------------------------------------

// configurableProvider implements every optional Configurable interface and
// records what it was handed. applyProviderOptions is pure type-assertion
// dispatch, so the only way it breaks is by wiring a value to the wrong
// setter — which a recorder catches and a compile check does not.
type configurableProvider struct {
	maxConcurrency int
	includeRaw     bool
	profile        string
	regions        []string
	compartments   []string

	kubeContext       string
	kubeContexts      []string
	kubeNamespace     string
	kubeExcludeNS     []string
	kubeExcludeHelm   bool
	kubeExcludeEvents bool

	netbirdURL      string
	gcpScope        string
	tailnet         string
	tailscaleAPIURL string
}

func (p *configurableProvider) Name() string                   { return "recorder" }
func (p *configurableProvider) Validate(context.Context) error { return nil }
func (p *configurableProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	a := make(chan core.Asset)
	e := make(chan error)
	close(a)
	close(e)
	return a, e
}

func (p *configurableProvider) SetMaxConcurrency(n int)             { p.maxConcurrency = n }
func (p *configurableProvider) SetIncludeRaw(b bool)                { p.includeRaw = b }
func (p *configurableProvider) SetProfile(s string)                 { p.profile = s }
func (p *configurableProvider) SetRegions(r []string)               { p.regions = r }
func (p *configurableProvider) SetCompartments(c []string)          { p.compartments = c }
func (p *configurableProvider) SetKubeContext(s string)             { p.kubeContext = s }
func (p *configurableProvider) SetKubeContexts(s []string)          { p.kubeContexts = s }
func (p *configurableProvider) SetKubeNamespace(s string)           { p.kubeNamespace = s }
func (p *configurableProvider) SetKubeExcludeNamespaces(s []string) { p.kubeExcludeNS = s }
func (p *configurableProvider) SetKubeExcludeHelmSecrets(b bool)    { p.kubeExcludeHelm = b }
func (p *configurableProvider) SetKubeExcludeEvents(b bool)         { p.kubeExcludeEvents = b }
func (p *configurableProvider) SetManagementURL(s string)           { p.netbirdURL = s }
func (p *configurableProvider) SetScope(s string)                   { p.gcpScope = s }
func (p *configurableProvider) SetTailnet(s string)                 { p.tailnet = s }
func (p *configurableProvider) SetAPIBaseURL(s string)              { p.tailscaleAPIURL = s }

func TestApplyProviderOptions_WiresEveryKnobToItsOwnSetter(t *testing.T) {
	p := &configurableProvider{}
	applyProviderOptions([]core.Provider{p}, providerOptions{
		maxConcurrency:         9,
		includeRaw:             true,
		ociProfile:             "PROD",
		ociRegions:             []string{"eu-frankfurt-1"},
		ociCompartments:        []string{"ocid1.compartment.oc1..c"},
		kubeContext:            "ctx",
		kubeContexts:           []string{"a", "b"},
		kubeNamespace:          "shop",
		kubeExcludeNamespaces:  []string{"kube-system"},
		kubeExcludeHelmSecrets: true,
		kubeExcludeEvents:      true,
		netbirdManagementURL:   "https://nb.example.com",
		gcpScope:               "projects/x",
		tailscaleTailnet:       "example.com",
		tailscaleAPIURL:        "https://headscale.internal",
	})

	// Each assertion pins one value to one setter — a swapped pair (the
	// realistic mistake, e.g. tailnet into the API URL) fails here.
	for _, tc := range []struct{ field, got, want string }{
		{"ociProfile", p.profile, "PROD"},
		{"kubeContext", p.kubeContext, "ctx"},
		{"kubeNamespace", p.kubeNamespace, "shop"},
		{"netbirdManagementURL", p.netbirdURL, "https://nb.example.com"},
		{"gcpScope", p.gcpScope, "projects/x"},
		{"tailscaleTailnet", p.tailnet, "example.com"},
		{"tailscaleAPIURL", p.tailscaleAPIURL, "https://headscale.internal"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.field, tc.got, tc.want)
		}
	}
	if p.maxConcurrency != 9 || !p.includeRaw {
		t.Errorf("maxConcurrency=%d includeRaw=%v", p.maxConcurrency, p.includeRaw)
	}
	if !p.kubeExcludeHelm || !p.kubeExcludeEvents {
		t.Error("kube exclude booleans not applied")
	}
	if strings.Join(p.kubeContexts, ",") != "a,b" {
		t.Errorf("kubeContexts = %v", p.kubeContexts)
	}
	if strings.Join(p.regions, ",") != "eu-frankfurt-1" {
		t.Errorf("regions = %v", p.regions)
	}
	if strings.Join(p.compartments, ",") != "ocid1.compartment.oc1..c" {
		t.Errorf("compartments = %v", p.compartments)
	}
}

// A provider that implements none of the optional interfaces must be skipped
// silently — they are knobs, not requirements.
func TestApplyProviderOptions_SkipsProvidersWithoutTheInterfaces(t *testing.T) {
	// A minimal provider value that satisfies only core.Provider.
	var p core.Provider = plainProvider{}
	// Must not panic and must not require anything of the provider.
	applyProviderOptions([]core.Provider{p}, providerOptions{maxConcurrency: 3})
}

type plainProvider struct{}

func (plainProvider) Name() string                   { return "plain" }
func (plainProvider) Validate(context.Context) error { return nil }
func (plainProvider) Collect(context.Context) (<-chan core.Asset, <-chan error) {
	a := make(chan core.Asset)
	e := make(chan error)
	close(a)
	close(e)
	return a, e
}

func TestResolveGCPScope(t *testing.T) {
	for _, tc := range []struct{ scope, project, want string }{
		// An explicit scope wins — it is the more specific instruction.
		{"organizations/123", "myproj", "organizations/123"},
		{"", "myproj", "projects/myproj"},
		{"folders/9", "", "folders/9"},
		// Empty means "leave the provider's env-derived default alone",
		// NOT "projects/" — that would be a scope that matches nothing.
		{"", "", ""},
	} {
		if got := resolveGCPScope(tc.scope, tc.project); got != tc.want {
			t.Errorf("resolveGCPScope(%q, %q) = %q, want %q", tc.scope, tc.project, got, tc.want)
		}
	}
}

func TestGraphProviderOptions_ForcesIncludeRaw(t *testing.T) {
	v := viper.New()
	v.Set("max-concurrency", 3)
	v.Set("gcp-project", "p1")
	v.Set("tailscale-tailnet", "t1")

	got := graphProviderOptions(v)
	// Non-negotiable: the Kubernetes resolvers parse Raw, and a graph built
	// without it silently loses whole classes of edge.
	if !got.includeRaw {
		t.Error("graphProviderOptions must force includeRaw on regardless of flags")
	}
	if got.maxConcurrency != 3 {
		t.Errorf("maxConcurrency = %d, want 3", got.maxConcurrency)
	}
	if got.gcpScope != "projects/p1" {
		t.Errorf("gcpScope = %q, want projects/p1", got.gcpScope)
	}
	if got.tailscaleTailnet != "t1" {
		t.Errorf("tailscaleTailnet = %q", got.tailscaleTailnet)
	}
}

// ----------------------------------------------------------------------
// provider selection
// ----------------------------------------------------------------------

func TestSelectProviders(t *testing.T) {
	t.Run("unknown name is skipped, not fatal", func(t *testing.T) {
		if got := selectProviders([]string{"definitely-not-a-provider"}); len(got) != 0 {
			t.Errorf("got %d providers for an unknown name", len(got))
		}
	})
	t.Run("none sentinel is case-insensitive", func(t *testing.T) {
		for _, name := range []string{"none", "None", "NONE"} {
			if got := selectProviders([]string{name}); len(got) != 0 {
				t.Errorf("selectProviders(%q) returned %d providers, want 0", name, len(got))
			}
		}
	})
	t.Run("blank entries are ignored", func(t *testing.T) {
		if got := selectProviders([]string{"", "  ", "none"}); len(got) != 0 {
			t.Errorf("got %d providers", len(got))
		}
	})
}

// ----------------------------------------------------------------------
// output plumbing
// ----------------------------------------------------------------------

func TestOpenOutput(t *testing.T) {
	t.Run("empty and dash both mean stdout", func(t *testing.T) {
		for _, path := range []string{"", "-"} {
			w, closeFn, err := openOutput(path)
			if err != nil {
				t.Fatalf("openOutput(%q) = %v", path, err)
			}
			if w != os.Stdout {
				t.Errorf("openOutput(%q) did not return stdout", path)
			}
			// The closer for stdout must be a no-op — closing the process's
			// stdout would break everything downstream of it.
			closeFn()
		}
	})

	t.Run("writes to a real file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "out.json")
		w, closeFn, err := openOutput(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte("hello")); err != nil {
			t.Fatal(err)
		}
		closeFn()

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != "hello" {
			t.Errorf("file contents = %q", got)
		}
	})

	t.Run("unwritable path is an error, not a panic", func(t *testing.T) {
		_, _, err := openOutput(filepath.Join(t.TempDir(), "no-such-dir", "out.json"))
		if err == nil {
			t.Error("expected an error for an unwritable path")
		}
	})
}

func TestIsCharDevice_RegularFileIsNot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if isCharDevice(f) {
		t.Error("a regular file must not be reported as a character device (that guard is what stops binary xlsx going to a TTY)")
	}
}

// ----------------------------------------------------------------------
// snapshot loading
// ----------------------------------------------------------------------

func TestLoadSnapshot(t *testing.T) {
	dir := t.TempDir()

	array := filepath.Join(dir, "array.json")
	if err := os.WriteFile(array, []byte(
		`[{"provider":"oci","type":"oci.instance","id":"i-1","name":"web"},
		  {"provider":"oci","type":"oci.instance","id":"i-2","name":"db"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	ndjson := filepath.Join(dir, "stream.ndjson")
	if err := os.WriteFile(ndjson, []byte(
		`{"provider":"oci","type":"oci.instance","id":"i-1","name":"web"}
{"provider":"oci","type":"oci.instance","id":"i-2","name":"db"}
`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Both shapes are what `audit -o json` emits (with and without --stream),
	// and the loader sniffs which it got.
	for name, path := range map[string]string{"array": array, "ndjson": ndjson} {
		t.Run(name, func(t *testing.T) {
			got, err := loadSnapshot(path)
			if err != nil {
				t.Fatalf("loadSnapshot: %v", err)
			}
			if len(got) != 2 || got[0].ID != "i-1" || got[1].Name != "db" {
				t.Errorf("loaded %d assets: %+v", len(got), got)
			}
		})
	}

	t.Run("missing file names the path", func(t *testing.T) {
		_, err := loadSnapshot(filepath.Join(dir, "nope.json"))
		if err == nil {
			t.Fatal("expected an error")
		}
		if !strings.Contains(err.Error(), "nope.json") {
			t.Errorf("error should name the file it failed on: %v", err)
		}
	})
}

// ----------------------------------------------------------------------
// reach question dispatch
// ----------------------------------------------------------------------

func reachTestTopology() *topology.Topology {
	dns := core.Asset{Provider: "cloudflare", Type: "cloudflare.dns_record", ID: "d1", Name: "api.example.com"}
	lb := core.Asset{Provider: "oci", Type: "oci.load_balancer", ID: "lb1", Name: "prod-lb"}
	pod := core.Asset{Provider: "kubernetes", Type: "v1.Pod", ID: "p1", Name: "api-pod"}
	return &topology.Topology{
		Nodes: []core.Asset{dns, lb, pod},
		Edges: []core.Edge{
			{From: dns.AsRef(), To: lb.AsRef(), Kind: core.EdgeKindDNS, Confidence: core.ConfidenceHeuristic},
			{From: lb.AsRef(), To: pod.AsRef(), Kind: core.EdgeKindLBBackend, Confidence: core.ConfidenceHeuristic},
		},
	}
}

func TestRunReach_DispatchesOnFlagCombination(t *testing.T) {
	topo := reachTestTopology()

	for _, tc := range []struct {
		name         string
		from, to     string
		exposed      bool
		wantQuestion string
		wantPaths    int
	}{
		{name: "exposed", exposed: true, wantQuestion: "internet", wantPaths: 2},
		{name: "downstream", from: "api.example.com", wantQuestion: "What can", wantPaths: 2},
		{name: "upstream", to: "api-pod", wantQuestion: "What can reach", wantPaths: 2},
		{name: "trace", from: "api.example.com", to: "api-pod", wantQuestion: "How can", wantPaths: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runReach(topo, tc.from, tc.to, tc.exposed, topology.ReachOptions{})
			if err != nil {
				t.Fatalf("runReach: %v", err)
			}
			if !strings.Contains(got.Question, tc.wantQuestion) {
				t.Errorf("Question = %q, want it to mention %q", got.Question, tc.wantQuestion)
			}
			if len(got.Paths) != tc.wantPaths {
				t.Errorf("got %d paths, want %d", len(got.Paths), tc.wantPaths)
			}
		})
	}
}

// A selector that matches nothing must be an error. An empty report would read
// as "nothing can reach it" — the opposite of "your selector was wrong".
func TestRunReach_UnmatchedSelectorIsAnError(t *testing.T) {
	topo := reachTestTopology()
	for _, tc := range []struct{ from, to string }{
		{from: "nope"},
		{to: "nope"},
		{from: "api.example.com", to: "nope"},
		{from: "nope", to: "api-pod"},
	} {
		_, err := runReach(topo, tc.from, tc.to, false, topology.ReachOptions{})
		if err == nil {
			t.Errorf("runReach(from=%q, to=%q) should have failed", tc.from, tc.to)
			continue
		}
		if !strings.Contains(err.Error(), "matched no assets") {
			t.Errorf("error should explain the selector matched nothing: %v", err)
		}
	}
}

// Truncation must be reported. A capped security result read as "these are all
// the ways in" is the dangerous kind of wrong.
func TestRunReach_ReportsTruncation(t *testing.T) {
	topo := reachTestTopology()
	got, err := runReach(topo, "api.example.com", "", false, topology.ReachOptions{MaxPaths: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated {
		t.Error("hitting MaxPaths must set Truncated")
	}

	full, err := runReach(topo, "api.example.com", "", false, topology.ReachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if full.Truncated {
		t.Error("an uncapped result must not claim truncation")
	}
}

func TestRequireMatch_SuggestsAWiderGlob(t *testing.T) {
	err := requireMatch("postgres", nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "*postgres*") {
		t.Errorf("error should suggest a wider glob: %v", err)
	}
	if requireMatch("x", []core.Asset{{ID: "x"}}) != nil {
		t.Error("a match must not error")
	}
}

// ----------------------------------------------------------------------
// command wiring
// ----------------------------------------------------------------------

// Every subcommand must construct, and the flags each one advertises in its
// help text must actually exist. A flag referenced in an example but never
// registered is a broken promise the compiler cannot catch.
func TestRootCommand_SubcommandsAndFlags(t *testing.T) {
	root := newRootCmd()

	want := map[string][]string{
		"audit":    {"provider", "output", "include-raw", "sheet-by", "summary", "cache", "cache-max-age", "tailscale-tailnet"},
		"topology": {"output", "group-by", "detail", "hostname", "include-orphans", "from-snapshot", "filter"},
		"reach":    {"from", "to", "exposed", "max-hops", "max-paths", "kinds", "include-deny", "exit-code", "from-snapshot"},
		"diff":     {"output", "exit-code"},
		"check":    {"rules", "exit-code", "fail-on"},
		"serve":    {"addr"},
		"cache":    nil,
		"secrets":  nil,
		"version":  nil,
	}

	byName := map[string]bool{}
	for _, c := range root.Commands() {
		byName[c.Name()] = true
	}
	for name, flags := range want {
		if !byName[name] {
			t.Errorf("subcommand %q is not registered on the root command", name)
			continue
		}
		if flags == nil {
			continue
		}
		cmd := findCmd(t, root, name)
		for _, f := range flags {
			if cmd.Flags().Lookup(f) == nil {
				t.Errorf("%s: flag --%s is not registered", name, f)
			}
		}
	}
}

// The graph commands share addGraphSourceFlags, so a provider knob added for
// one must reach the other. They drifted apart once already.
func TestGraphCommands_ShareTheSameProviderFlags(t *testing.T) {
	root := newRootCmd()
	topo, reach := findCmd(t, root, "topology"), findCmd(t, root, "reach")

	for _, f := range []string{
		"provider", "from-snapshot", "max-concurrency", "timeout",
		"oci-profile", "oci-regions", "oci-compartments",
		"kube-context", "kube-contexts", "kube-namespace",
		"netbird-management-url", "gcp-scope", "gcp-project",
		"tailscale-tailnet", "tailscale-api-url",
	} {
		if topo.Flags().Lookup(f) == nil {
			t.Errorf("topology is missing --%s", f)
		}
		if reach.Flags().Lookup(f) == nil {
			t.Errorf("reach is missing --%s", f)
		}
	}
}

// `reach` with no selector must explain itself rather than silently doing
// nothing or dumping the whole graph.
func TestReachCmd_RequiresASelector(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"reach", "--from-snapshot", "/dev/null"})
	root.SetOut(&strings.Builder{})
	root.SetErr(&strings.Builder{})

	err := root.Execute()
	if err == nil {
		t.Fatal("expected an error when no selector is given")
	}
	if !strings.Contains(err.Error(), "--from") {
		t.Errorf("error should point at the selector flags: %v", err)
	}
}

func findCmd(t *testing.T, root *cobra.Command, name string) *cobra.Command {
	t.Helper()
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("subcommand %q not found", name)
	return nil
}

// ----------------------------------------------------------------------
// sentinel errors
// ----------------------------------------------------------------------

// Execute() maps ErrPartial to exit code 2 and everything else to 1, by
// errors.Is. Wrapping must preserve that — a joined error that loses the
// sentinel silently downgrades "some providers failed" to a plain failure.
func TestSentinels_SurviveJoining(t *testing.T) {
	joined := errors.Join(append([]error{ErrPartial}, errors.New("oci timed out"))...)
	if !errors.Is(joined, ErrPartial) {
		t.Error("ErrPartial must survive errors.Join — Execute() maps it to exit code 2")
	}

	exposed := errors.Join(ErrExposed, nil)
	if !errors.Is(exposed, ErrExposed) {
		t.Error("ErrExposed must survive being joined with a nil error list")
	}
	// The two must stay distinguishable, or a partial audit would be reported
	// as an exposure finding.
	if errors.Is(exposed, ErrPartial) {
		t.Error("ErrExposed must not satisfy errors.Is(_, ErrPartial)")
	}
}

// ----------------------------------------------------------------------
// provider fan-in (runProviders / forward)
// ----------------------------------------------------------------------

// streamProvider emits a fixed number of assets and errors, optionally blocking
// forever so cancellation can be exercised.
type streamProvider struct {
	name    string
	assets  int
	errs    int
	blockOn chan struct{} // when non-nil, Collect blocks on this before closing
}

func (f streamProvider) Name() string                   { return f.name }
func (f streamProvider) Validate(context.Context) error { return nil }

func (f streamProvider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error)
	go func() {
		defer close(assets)
		defer close(errs)
		for i := range f.assets {
			select {
			case assets <- core.Asset{Provider: f.name, Type: "t", ID: strconv.Itoa(i), Name: "a"}:
			case <-ctx.Done():
				return
			}
		}
		for i := range f.errs {
			select {
			case errs <- fmt.Errorf("%s error %d", f.name, i):
			case <-ctx.Done():
				return
			}
		}
		if f.blockOn != nil {
			select {
			case <-f.blockOn:
			case <-ctx.Done():
			}
		}
	}()
	return assets, errs
}

// drain consumes both channels concurrently. Reading them sequentially would
// deadlock: an unbuffered error channel blocks the producer until someone
// reads it, and the producer is the same goroutine feeding assets.
func drain(assets <-chan core.Asset, errs <-chan error) ([]core.Asset, []error) {
	var (
		gotAssets []core.Asset
		gotErrs   []error
		done      = make(chan struct{})
	)
	go func() {
		for e := range errs {
			gotErrs = append(gotErrs, e)
		}
		close(done)
	}()
	for a := range assets {
		gotAssets = append(gotAssets, a)
	}
	<-done
	return gotAssets, gotErrs
}

func TestRunProviders_FansInEveryProvider(t *testing.T) {
	assets, errs := runProviders(context.Background(), []core.Provider{
		streamProvider{name: "a", assets: 3, errs: 1},
		streamProvider{name: "b", assets: 2, errs: 2},
	})
	gotAssets, gotErrs := drain(assets, errs)

	if len(gotAssets) != 5 {
		t.Errorf("got %d assets, want 5", len(gotAssets))
	}
	if len(gotErrs) != 3 {
		t.Errorf("got %d errors, want 3", len(gotErrs))
	}
	// Partial failure is normal (invariant 5): a provider that errored must
	// still have contributed its assets.
	perProvider := map[string]int{}
	for _, a := range gotAssets {
		perProvider[a.Provider]++
	}
	if perProvider["a"] != 3 || perProvider["b"] != 2 {
		t.Errorf("per-provider counts = %v, want a=3 b=2", perProvider)
	}
}

// Both channels must close exactly once, even with zero providers — the
// renderer blocks on them, so a channel left open hangs the whole command.
func TestRunProviders_ClosesBothChannelsWithNoProviders(t *testing.T) {
	assets, errs := runProviders(context.Background(), nil)
	gotAssets, gotErrs := drain(assets, errs)
	if len(gotAssets) != 0 || len(gotErrs) != 0 {
		t.Errorf("expected nothing, got %d assets and %d errors", len(gotAssets), len(gotErrs))
	}
	// drain returning at all proves both channels closed; a second receive
	// confirms they are closed rather than merely empty.
	if _, open := <-assets; open {
		t.Error("asset channel is still open")
	}
	if _, open := <-errs; open {
		t.Error("error channel is still open")
	}
}

// Ctrl+C must stop work promptly (invariant 2). A cancelled context has to
// close the fan-in even while a provider is still blocked.
func TestRunProviders_CancellationClosesChannels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	block := make(chan struct{})
	defer close(block)

	assets, errs := runProviders(ctx, []core.Provider{
		streamProvider{name: "slow", assets: 1, blockOn: block},
	})

	done := make(chan struct{})
	go func() {
		drain(assets, errs)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runProviders did not close its channels within 5s of cancellation")
	}
}

// ----------------------------------------------------------------------
// gatherForGraph
// ----------------------------------------------------------------------

func TestGatherForGraph_SnapshotPathSkipsProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "snap.json")
	if err := os.WriteFile(path, []byte(
		`[{"provider":"oci","type":"oci.instance","id":"i-1","name":"web"}]`), 0o600); err != nil {
		t.Fatal(err)
	}

	v := viper.New()
	v.Set("from-snapshot", path)
	// Deliberately also select a provider: the snapshot must win, so the
	// command stays instant and makes zero API calls.
	v.Set("provider", []string{"cloudflare"})

	s := &cliState{v: v}
	got, provErrs, err := s.gatherForGraph(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "i-1" {
		t.Errorf("loaded %+v", got)
	}
	if len(provErrs) != 0 {
		t.Errorf("snapshot path must not produce provider errors: %v", provErrs)
	}
}

func TestGatherForGraph_MissingSnapshotIsFatal(t *testing.T) {
	v := viper.New()
	v.Set("from-snapshot", filepath.Join(t.TempDir(), "nope.json"))
	s := &cliState{v: v}
	if _, _, err := s.gatherForGraph(context.Background(), v); err == nil {
		t.Error("a missing snapshot must be a fatal error, not an empty graph")
	}
}

func TestGatherForGraph_LiveWithNoProvidersYieldsEmpty(t *testing.T) {
	v := viper.New()
	v.Set("provider", []string{"none"})
	v.Set("timeout", time.Minute)

	s := &cliState{v: v}
	got, provErrs, err := s.gatherForGraph(context.Background(), v)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || len(provErrs) != 0 {
		t.Errorf("got %d assets, %d errors; want none", len(got), len(provErrs))
	}
}
