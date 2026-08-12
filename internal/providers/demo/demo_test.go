package demo

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/topology"
)

// fastProvider is the provider with pacing disabled — tests care about the
// contract and the fixture, not about the three seconds of theatre.
func fastProvider(includeRaw bool) *Provider {
	p := New(Config{StreamDuration: -1})
	p.SetIncludeRaw(includeRaw)
	p.SetMaxConcurrency(9) // accepted and ignored; must not panic
	return p
}

func drain(t *testing.T, p *Provider, ctx context.Context) ([]core.Asset, []error) {
	t.Helper()
	assetsCh, errsCh := p.Collect(ctx)

	var (
		assets []core.Asset
		errs   []error
	)
	for assetsCh != nil || errsCh != nil {
		select {
		case a, ok := <-assetsCh:
			if !ok {
				assetsCh = nil
				continue
			}
			assets = append(assets, a)
		case e, ok := <-errsCh:
			if !ok {
				errsCh = nil
				continue
			}
			errs = append(errs, e)
		case <-time.After(10 * time.Second):
			t.Fatal("Collect did not finish within 10s")
		}
	}
	return assets, errs
}

func TestCollect_ClosesBothChannelsExactlyOnce(t *testing.T) {
	assets, errs := drain(t, fastProvider(false), context.Background())

	if len(assets) == 0 {
		t.Fatal("collected no assets")
	}
	if len(errs) != len(demoErrors) {
		t.Fatalf("got %d errors, want %d (the UI's partial-failure state needs some)", len(errs), len(demoErrors))
	}

	// A second receive on a closed channel returns immediately with ok=false;
	// a double close would have panicked inside Collect's goroutine already,
	// so reaching here with both channels drained is the assertion. Both are
	// drained concurrently: the errors are interleaved with the assets, so
	// draining one at a time wedges the producer on the other.
	ch, ech := fastProvider(false).Collect(context.Background())
	drained := make(chan struct{})
	go func() {
		for range ech { //nolint:revive // draining
		}
		close(drained)
	}()
	for range ch { //nolint:revive // draining
	}
	<-drained
	if _, ok := <-ch; ok {
		t.Error("asset channel yielded a value after close")
	}
	if _, ok := <-ech; ok {
		t.Error("error channel yielded a value after close")
	}
}

func TestCollect_RespectsContextCancellation(t *testing.T) {
	// Pace the stream so cancellation lands mid-run rather than after it.
	p := New(Config{StreamDuration: 30 * time.Second})
	ctx, cancel := context.WithCancel(context.Background())

	assetsCh, errsCh := p.Collect(ctx)
	<-assetsCh // one asset proves the stream started
	cancel()

	done := make(chan struct{})
	go func() {
		for range assetsCh { //nolint:revive // draining
		}
		for range errsCh { //nolint:revive // draining
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Collect did not stop and close its channels within 2s of cancellation")
	}
}

func TestCollect_IncludeRawGatesPayloads(t *testing.T) {
	without, _ := drain(t, fastProvider(false), context.Background())
	for _, a := range without {
		if len(a.Raw) != 0 {
			t.Fatalf("asset %s/%s carried Raw without --include-raw", a.Provider, a.ID)
		}
	}

	with, _ := drain(t, fastProvider(true), context.Background())
	var withRaw int
	for _, a := range with {
		if len(a.Raw) > 0 {
			withRaw++
		}
	}
	if withRaw != len(with) {
		t.Fatalf("%d of %d assets carried Raw with --include-raw; want all", withRaw, len(with))
	}
}

func TestFixture_AssetsAreWellFormedAndUnique(t *testing.T) {
	assets := Assets()

	if n := len(assets); n < 450 || n > 700 {
		t.Errorf("fixture has %d assets; the demo is sized for 450-700", n)
	}

	seen := make(map[string]core.Asset, len(assets))
	for _, a := range assets {
		switch {
		case a.Provider == "":
			t.Errorf("asset %q has no provider", a.ID)
		case a.Type == "":
			t.Errorf("asset %q (%s) has no type", a.ID, a.Provider)
		case a.ID == "":
			t.Errorf("asset %q (%s) has no id", a.Name, a.Provider)
		case a.Name == "":
			t.Errorf("asset %q (%s) has no name", a.ID, a.Provider)
		case a.AccountID == "":
			t.Errorf("asset %q (%s) has no account id", a.ID, a.Provider)
		}
		key := a.Provider + "/" + a.ID
		if prev, dup := seen[key]; dup {
			t.Errorf("duplicate asset id %s: %q and %q", key, prev.Name, a.Name)
		}
		seen[key] = a
	}

	// Every provider the demo claims to model must actually be present —
	// a silently-dropped section would quietly shrink the graph.
	want := []string{"cloudflare", "gcp", "kubernetes", "netbird", "oci", "tailscale"}
	byProvider := map[string]int{}
	for _, a := range assets {
		byProvider[a.Provider]++
	}
	for _, p := range want {
		if byProvider[p] == 0 {
			t.Errorf("fixture contains no %s assets", p)
		}
	}
}

func TestFixture_IsByteStableAcrossRuns(t *testing.T) {
	first, err := json.Marshal(Assets())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	second, err := json.Marshal(Assets())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("fixture is not deterministic: two builds serialised differently")
	}

	// And the streamed order must be stable too — a diff between two demo
	// snapshots has to come out empty.
	a1, _ := drain(t, fastProvider(true), context.Background())
	a2, _ := drain(t, fastProvider(true), context.Background())
	b1, _ := json.Marshal(a1)
	b2, _ := json.Marshal(a2)
	if string(b1) != string(b2) {
		t.Fatal("two Collect runs produced different streams")
	}
}

// TestFixture_ExercisesEveryEdgeKind is the reason the demo fixture is worth
// maintaining: it is the only place in the repo where all nine resolvers run
// against one coherent inventory. If a resolver stops producing an edge, the
// fixture is wrong (or the resolver regressed) — fix that, don't relax this.
func TestFixture_ExercisesEveryEdgeKind(t *testing.T) {
	assets, _ := drain(t, fastProvider(true), context.Background())

	graph := topology.Build(assets)

	byKind := map[string]int{}
	for _, e := range graph.Edges {
		byKind[e.Kind]++
	}

	wantKinds := []string{
		core.EdgeKindDNS,
		core.EdgeKindWAF,
		core.EdgeKindLBBackend,
		core.EdgeKindGatewayRoute,
		core.EdgeKindServiceBackend,
		core.EdgeKindNetworkContainment,
		core.EdgeKindTrafficAllow,
		core.EdgeKindTrafficDeny,
	}
	for _, k := range wantKinds {
		if byKind[k] == 0 {
			t.Errorf("no %s edge in the demo topology", k)
		}
	}

	// Report the coverage table so a failure (or a curious reader) sees the
	// whole picture, not just the missing row.
	kinds := make([]string, 0, len(byKind))
	for k := range byKind {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	var b strings.Builder
	b.WriteString("edge coverage:\n")
	for _, k := range kinds {
		b.WriteString("  " + k + ": " + itoa(byKind[k]) + "\n")
	}
	b.WriteString("  (nodes: " + itoa(len(graph.Nodes)) + ", edges: " + itoa(len(graph.Edges)) + ")")
	t.Log(b.String())
}

// TestFixture_TrafficEdgeCarriesPort pins the one selector form that is easy
// to get wrong: a Tailscale dst of "tag:prod:22" must yield Edge.Port 22, not
// a host named "tag" nor a dropped rule.
func TestFixture_TrafficEdgeCarriesPort(t *testing.T) {
	assets, _ := drain(t, fastProvider(true), context.Background())
	for _, e := range topology.Build(assets).Edges {
		if e.Kind == core.EdgeKindTrafficAllow && e.Port == 22 {
			return
		}
	}
	t.Error("no traffic-allow edge carries port 22; the tag:prod:22 ACL rule stopped resolving")
}

func TestErrorOffsets_AreInRangeAndDistinct(t *testing.T) {
	const n = 500
	offsets := errorOffsets(n)
	if len(offsets) != len(demoErrors) {
		t.Fatalf("got %d offsets, want %d", len(offsets), len(demoErrors))
	}
	for at, msg := range offsets {
		if at < 0 || at >= n {
			t.Errorf("offset %d for %q is outside the stream", at, msg)
		}
	}
	// errorOffsets on an empty stream must not divide by zero.
	if got := errorOffsets(0); len(got) != 0 {
		t.Errorf("errorOffsets(0) = %v, want empty", got)
	}
}

func TestPerAssetPause(t *testing.T) {
	if got := perAssetPause(-1, 100); got != 0 {
		t.Errorf("negative budget = %v, want 0 (pacing disabled)", got)
	}
	if got := perAssetPause(time.Second, 0); got != 0 {
		t.Errorf("zero assets = %v, want 0", got)
	}
	if got := perAssetPause(3*time.Second, 600); got != 5*time.Millisecond {
		t.Errorf("perAssetPause(3s, 600) = %v, want 5ms", got)
	}
}
