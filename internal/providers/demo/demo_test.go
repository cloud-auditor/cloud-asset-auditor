package demo

import (
	"context"
	"encoding/json"
	"net/netip"
	"sort"
	"strconv"
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

// TestFixture_PodMetricsJoinTheirPods pins the join that makes usage data
// usable at all: a PodMetrics is matched to a Pod by (cluster, namespace,
// name) and by nothing else, because that tuple is all the metrics API
// publishes. It also pins the two deliberate holes — the Pending pod and the
// whole staging cluster have no metrics — since a fixture where every pod had
// a reading would let a consumer treat "no metrics" as unreachable.
func TestFixture_PodMetricsJoinTheirPods(t *testing.T) {
	assets := Assets()

	pods := map[string]core.Asset{}
	for _, a := range assets {
		if a.Type == "v1.Pod" {
			pods[a.AccountID+"/"+a.Tags["namespace"]+"/"+a.Name] = a
		}
	}

	var metrics int
	for _, a := range assets {
		if a.Type != "metrics.k8s.io/v1beta1.PodMetrics" {
			continue
		}
		metrics++
		pod, ok := pods[a.AccountID+"/"+a.Tags["namespace"]+"/"+a.Name]
		if !ok {
			t.Errorf("PodMetrics %s/%s has no matching Pod; the join every usage finding depends on is broken",
				a.Tags["namespace"], a.Name)
			continue
		}
		if pod.Status != "Running" {
			t.Errorf("PodMetrics for %s, which is %s: the metrics API holds no entry for a pod that isn't running", a.Name, pod.Status)
		}
		if a.AccountID != kubeProdCluster {
			t.Errorf("PodMetrics %s is on the staging cluster, which has no metrics-server (see demoErrors)", a.Name)
		}
	}

	if metrics == 0 {
		t.Fatal("no PodMetrics in the fixture")
	}
	if metrics >= len(pods) {
		t.Errorf("%d PodMetrics for %d pods: the fixture must keep pods with no observation, or 'we cannot tell' has no example", metrics, len(pods))
	}
}

// TestFixture_NodeAccountingIsPhysicallyPossible reads the emitted Raw with a
// parser other than the one that wrote it and checks the fixture describes a
// cluster that could exist: no node has more requested of it than it can hand
// out, and every node uses more than its pods do, because the kubelet and the
// OS belong to no pod.
func TestFixture_NodeAccountingIsPhysicallyPossible(t *testing.T) {
	assets := Assets()

	type resources struct{ cpuMilli, memKi int64 }
	allocatable := map[string]resources{}
	requested := map[string]resources{}
	podUsage := map[string]resources{}  // pod name → usage
	podNode := map[string]string{}      // pod name → node
	nodeUsage := map[string]resources{} // node name → usage
	nodeCluster := map[string]string{}

	for _, a := range assets {
		switch a.Type {
		case "v1.Node":
			var obj struct {
				Status struct {
					Allocatable map[string]string `json:"allocatable"`
				} `json:"status"`
			}
			mustJSON(t, a.Raw, &obj)
			allocatable[a.Name] = resources{
				cpuMilli: parseCPUMillis(t, obj.Status.Allocatable["cpu"]),
				memKi:    parseMemKi(t, obj.Status.Allocatable["memory"]),
			}
			nodeCluster[a.Name] = a.AccountID

		case "v1.Pod":
			var obj struct {
				Spec struct {
					NodeName   string `json:"nodeName"`
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			}
			mustJSON(t, a.Raw, &obj)
			if obj.Spec.NodeName == "" {
				t.Errorf("pod %s names no node", a.Name)
				continue
			}
			podNode[a.Name] = obj.Spec.NodeName
			r := requested[obj.Spec.NodeName]
			for _, c := range obj.Spec.Containers {
				r.cpuMilli += parseCPUMillis(t, c.Resources.Requests["cpu"])
				r.memKi += parseMemKi(t, c.Resources.Requests["memory"])
			}
			requested[obj.Spec.NodeName] = r

		case "metrics.k8s.io/v1beta1.PodMetrics":
			var obj struct {
				Containers []struct {
					Usage map[string]string `json:"usage"`
				} `json:"containers"`
			}
			mustJSON(t, a.Raw, &obj)
			var r resources
			for _, c := range obj.Containers {
				r.cpuMilli += parseCPUMillis(t, c.Usage["cpu"])
				r.memKi += parseMemKi(t, c.Usage["memory"])
			}
			podUsage[a.Name] = r

		case "metrics.k8s.io/v1beta1.NodeMetrics":
			var obj struct {
				Usage map[string]string `json:"usage"`
			}
			mustJSON(t, a.Raw, &obj)
			nodeUsage[a.Name] = resources{
				cpuMilli: parseCPUMillis(t, obj.Usage["cpu"]),
				memKi:    parseMemKi(t, obj.Usage["memory"]),
			}
		}
	}

	if len(allocatable) == 0 {
		t.Fatal("no v1.Node assets carry status.allocatable")
	}

	for node, alloc := range allocatable {
		req := requested[node]
		if req.cpuMilli > alloc.cpuMilli {
			t.Errorf("%s: pods request %dm of %dm allocatable — the scheduler could not have placed them", node, req.cpuMilli, alloc.cpuMilli)
		}
		if req.memKi > alloc.memKi {
			t.Errorf("%s: pods request %dKi of %dKi allocatable", node, req.memKi, alloc.memKi)
		}
	}

	var checked int
	for node, used := range nodeUsage {
		var podSum resources
		for pod, n := range podNode {
			if n != node {
				continue
			}
			podSum.cpuMilli += podUsage[pod].cpuMilli
			podSum.memKi += podUsage[pod].memKi
		}
		checked++
		if used.cpuMilli <= podSum.cpuMilli {
			t.Errorf("%s reports %dm but its pods sum to %dm: a node always costs more than the pods on it", node, used.cpuMilli, podSum.cpuMilli)
		}
		if used.memKi <= podSum.memKi {
			t.Errorf("%s reports %dKi but its pods sum to %dKi", node, used.memKi, podSum.memKi)
		}
	}
	if checked == 0 {
		t.Error("no NodeMetrics to check against pod usage")
	}
}

// TestFixture_PlantedShapesAreStillThere guards the cases the fixture exists
// to contain. Every one of them is easy to tidy away by accident — renaming a
// boot volume, "fixing" the overlapping CIDRs, giving the untagged resources
// an owner — and every one of them, once gone, makes the feature that reads it
// demo as empty. An empty result looks like a broken feature, not a changed
// fixture, which is why these are assertions and not a comment.
func TestFixture_PlantedShapesAreStillThere(t *testing.T) {
	assets := Assets()
	graph := topology.Build(assets)

	hasEdgeFrom := map[string]bool{}
	for _, e := range graph.Edges {
		hasEdgeFrom[e.From.ID] = true
	}
	byName := func(typ, name string) (core.Asset, bool) {
		for _, a := range assets {
			if a.Type == typ && a.Name == name {
				return a, true
			}
		}
		return core.Asset{}, false
	}

	t.Run("dangling DNS record", func(t *testing.T) {
		anchor, ok := byName("cloudflare.dns_record", "www.northwind.example")
		if !ok {
			t.Fatal("the anchor record is gone")
		}
		// Asserted alongside the dangling one so a resolver regression fails
		// here as a resolver regression, not as a missing fixture record.
		if !hasEdgeFrom[anchor.ID] {
			t.Error("the anchor record resolves to nothing; dnsToTarget regressed")
		}
		dangling, ok := byName("cloudflare.dns_record", "checkout-old.northwind.example")
		if !ok {
			t.Fatal("the dangling A record is gone")
		}
		if hasEdgeFrom[dangling.ID] {
			t.Error("checkout-old.northwind.example resolves to something; it is meant to point at nothing")
		}
	})

	t.Run("overlapping VCN CIDRs", func(t *testing.T) {
		type vcn struct {
			name   string
			prefix netip.Prefix
		}
		var vcns []vcn
		for _, a := range assets {
			if a.Type != "oci.vcn" {
				continue
			}
			p, err := netip.ParsePrefix(a.Tags["cidr_blocks"])
			if err != nil {
				t.Errorf("vcn %s has an unparseable cidr_blocks tag %q", a.Name, a.Tags["cidr_blocks"])
				continue
			}
			vcns = append(vcns, vcn{a.Name, p})
		}
		var overlaps int
		for i := range vcns {
			for j := i + 1; j < len(vcns); j++ {
				if vcns[i].prefix.Overlaps(vcns[j].prefix) {
					overlaps++
				}
			}
		}
		if overlaps < 2 {
			t.Errorf("found %d overlapping VCN pairs, want at least 2 (an exact collision and a partial one)", overlaps)
		}
	})

	t.Run("subnet CIDRs are present and varied", func(t *testing.T) {
		sizes := map[int]bool{}
		for _, a := range assets {
			if a.Type != "oci.subnet" {
				continue
			}
			p, err := netip.ParsePrefix(a.Tags["cidr_block"])
			if err != nil {
				t.Errorf("subnet %s has an unparseable cidr_block tag %q", a.Name, a.Tags["cidr_block"])
				continue
			}
			sizes[p.Bits()] = true
		}
		if len(sizes) < 2 {
			t.Error("every subnet is the same size; VCN address utilisation would be one number for the whole tenancy")
		}
	})

	t.Run("NAT gateway with an empty compartment behind it", func(t *testing.T) {
		occupied := map[string]bool{}
		for _, a := range assets {
			if a.Provider == "oci" && (a.Type == "oci.compute.instance" || a.Type == "oci.oke.cluster" || a.Type == "oci.network_load_balancer") {
				occupied[a.Tags["compartment_id"]] = true
			}
		}
		var idle int
		for _, a := range assets {
			if a.Type == "oci.nat_gateway" && !occupied[a.Tags["compartment_id"]] {
				idle++
			}
		}
		if idle == 0 {
			t.Error("every NAT gateway shares a compartment with something that could be using it")
		}
	})

	t.Run("stopped instance with a boot volume", func(t *testing.T) {
		boots := map[string]bool{}
		for _, a := range assets {
			if a.Type == "oci.boot_volume" {
				boots[a.Name] = true
			}
		}
		var found int
		for _, a := range assets {
			if a.Type == "oci.compute.instance" && a.Status == "STOPPED" && boots[a.Name] {
				found++
			}
		}
		if found == 0 {
			t.Error("no stopped instance shares a name with a boot volume; the only link between the two is that name")
		}
	})

	t.Run("a handful of untagged resources", func(t *testing.T) {
		var tagged, untagged int
		for _, a := range assets {
			switch a.Type {
			case "oci.compute.instance", "oci.block_volume", "oci.object_storage.bucket", "compute.googleapis.com/Instance":
				if a.Tags["owner"] == "" {
					untagged++
				} else {
					tagged++
				}
			}
		}
		if untagged == 0 {
			t.Error("everything is owned; a tagging gap cannot be demonstrated")
		}
		if untagged > tagged/2 {
			t.Errorf("%d of %d taggable resources have no owner: that is a convention nobody adopted, not a gap", untagged, tagged+untagged)
		}
	})

	t.Run("a certificate near expiry", func(t *testing.T) {
		var withExpiry, soon int
		for _, a := range assets {
			raw := a.Tags["expires_on"]
			if raw == "" {
				continue
			}
			withExpiry++
			exp, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				t.Errorf("%s has an unparseable expires_on %q", a.Name, raw)
				continue
			}
			// Measured from the fixture's frozen clock, not from now — see
			// expiresAt. Against wall-clock time every one of these is years
			// past, which is the correct reading of a two-year-old snapshot.
			if d := exp.Sub(fixtureEpoch); d > 0 && d < 14*24*time.Hour {
				soon++
			}
		}
		if withExpiry == 0 {
			t.Fatal("no asset carries an expires_on tag")
		}
		if soon == 0 {
			t.Error("no certificate expires within 14 days of the fixture epoch")
		}
	})

	t.Run("pods that cannot be judged", func(t *testing.T) {
		var noRequests int
		for _, a := range assets {
			if a.Type != "v1.Pod" {
				continue
			}
			var obj struct {
				Spec struct {
					Containers []struct {
						Resources struct {
							Requests map[string]string `json:"requests"`
						} `json:"resources"`
					} `json:"containers"`
				} `json:"spec"`
			}
			mustJSON(t, a.Raw, &obj)
			for _, c := range obj.Spec.Containers {
				if len(c.Resources.Requests) == 0 {
					noRequests++
				}
			}
		}
		if noRequests == 0 {
			t.Error("every pod sets requests; 'over-requested relative to what?' has no example")
		}
	})
}

func mustJSON(t *testing.T, raw json.RawMessage, into any) {
	t.Helper()
	if len(raw) == 0 {
		t.Fatal("asset carries no Raw; Assets() must always populate it")
	}
	if err := json.Unmarshal(raw, into); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
}

// parseCPUMillis and parseMemKi read the subset of Kubernetes quantity
// notation the fixture uses. They are hand-rolled, and narrow on purpose: the
// point of the accounting test is to read the fixture with something other
// than the code that wrote it, so borrowing a shared helper would make the
// test agree with the fixture by construction.
func parseCPUMillis(t *testing.T, q string) int64 {
	t.Helper()
	if q == "" {
		return 0
	}
	if strings.HasSuffix(q, "m") {
		return atoi64(t, strings.TrimSuffix(q, "m"))
	}
	return atoi64(t, q) * 1000
}

func parseMemKi(t *testing.T, q string) int64 {
	t.Helper()
	if q == "" {
		return 0
	}
	for suffix, mult := range map[string]int64{"Ki": 1, "Mi": 1024, "Gi": 1024 * 1024} {
		if strings.HasSuffix(q, suffix) {
			return atoi64(t, strings.TrimSuffix(q, suffix)) * mult
		}
	}
	// A bare memory quantity is bytes.
	return atoi64(t, q) / 1024
}

func atoi64(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatalf("unparseable quantity %q: %v", s, err)
	}
	return n
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
