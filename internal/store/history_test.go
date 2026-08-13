package store

// White-box like assets_test.go: run_at is stamped inside SaveAudit, so the
// only way to build a multi-day history is to backdate rows through s.db.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// seedSeries saves one snapshot and backdates it by ago.
func seedSeries(t *testing.T, st *Store, ago time.Duration, providers []string, assets ...core.Asset) int64 {
	t.Helper()
	id, err := st.SaveAudit(context.Background(), providers, assets)
	if err != nil {
		t.Fatalf("SaveAudit: %v", err)
	}
	if ago > 0 {
		backdateAudit(t, st, id, ago)
	}
	return id
}

func peer(id, name, status string) core.Asset {
	return core.Asset{Provider: "netbird", Type: "netbird.peer", ID: id, Name: name, Status: status}
}

func TestAuditBefore_PicksNewestAtOrBefore(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	oldest := seedSeries(t, st, 90*24*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	middle := seedSeries(t, st, 30*24*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))
	newest := seedSeries(t, st, 0, []string{"netbird"}, peer("p1", "gw", "connected"))

	// 20 days ago: the 30-day-old snapshot is the newest one at or before it.
	got, err := st.AuditBefore(ctx, nil, time.Now().Add(-20*24*time.Hour))
	if err != nil {
		t.Fatalf("AuditBefore: %v", err)
	}
	if got.ID != middle {
		t.Errorf("AuditBefore(20d ago) = %d, want %d", got.ID, middle)
	}

	// Now: the newest snapshot qualifies (at-or-before includes "now").
	got, err = st.AuditBefore(ctx, nil, time.Now())
	if err != nil {
		t.Fatalf("AuditBefore(now): %v", err)
	}
	if got.ID != newest {
		t.Errorf("AuditBefore(now) = %d, want %d", got.ID, newest)
	}

	// Older than everything: not found, so the caller can say how far back
	// the history goes rather than rounding forwards to `oldest`.
	if _, err := st.AuditBefore(ctx, nil, time.Now().Add(-365*24*time.Hour)); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("AuditBefore(1y ago) err = %v, want ErrAuditNotFound", err)
	}

	if o, err := st.OldestAudit(ctx, nil); err != nil || o.ID != oldest {
		t.Errorf("OldestAudit = (%d, %v), want (%d, nil)", o.ID, err, oldest)
	}
	if n, err := st.NewestAudit(ctx, nil); err != nil || n.ID != newest {
		t.Errorf("NewestAudit = (%d, %v), want (%d, nil)", n.ID, err, newest)
	}
}

func TestAuditSelectors_ScopeToProviderSet(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	full := seedSeries(t, st, 48*time.Hour, []string{"netbird", "cloudflare"}, peer("p1", "gw", "connected"))
	partial := seedSeries(t, st, 1*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))

	// Unscoped: the netbird-only run is newest.
	if n, err := st.NewestAudit(ctx, nil); err != nil || n.ID != partial {
		t.Errorf("NewestAudit(any) = (%d, %v), want %d", n.ID, err, partial)
	}
	// Scoped to the full set — and the provider list is canonicalized, so
	// the flag order the caller happens to use must not matter.
	n, err := st.NewestAudit(ctx, []string{"cloudflare", "netbird"})
	if err != nil || n.ID != full {
		t.Fatalf("NewestAudit(full set) = (%d, %v), want %d", n.ID, err, full)
	}
	if n2, err := st.NewestAudit(ctx, []string{"NetBird", "Cloudflare"}); err != nil || n2.ID != full {
		t.Errorf("NewestAudit is not canonicalizing the provider set: got (%d, %v)", n2.ID, err)
	}
	if _, err := st.NewestAudit(ctx, []string{"oci"}); !errors.Is(err, ErrAuditNotFound) {
		t.Errorf("NewestAudit(unseen set) err = %v, want ErrAuditNotFound", err)
	}
}

func TestAuditSelectors_EmptyStore(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()
	for name, call := range map[string]func() (AuditMeta, error){
		"NewestAudit": func() (AuditMeta, error) { return st.NewestAudit(ctx, nil) },
		"OldestAudit": func() (AuditMeta, error) { return st.OldestAudit(ctx, nil) },
		"AuditBefore": func() (AuditMeta, error) { return st.AuditBefore(ctx, nil, time.Now()) },
	} {
		if _, err := call(); !errors.Is(err, ErrAuditNotFound) {
			t.Errorf("%s on empty store err = %v, want ErrAuditNotFound", name, err)
		}
	}
}

func TestMatchAssetIdentities_GlobsIDAndName(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	seedSeries(t, st, 24*time.Hour, []string{"netbird", "cloudflare"},
		peer("p1", "gw-prod", "connected"),
		peer("p2", "gw-dev", "connected"),
		core.Asset{Provider: "cloudflare", Type: "cloudflare.zone", ID: "z1", Name: "example.com"},
	)
	// A later snapshot renames p1: the identity must not double up.
	seedSeries(t, st, 0, []string{"netbird", "cloudflare"},
		peer("p1", "gateway-prod", "connected"),
	)

	cases := []struct {
		selector string
		want     []AssetIdentity
	}{
		{"p1", []AssetIdentity{{"netbird", "p1"}}},
		{"P1", []AssetIdentity{{"netbird", "p1"}}}, // case-insensitive on id
		{"gw-*", []AssetIdentity{{"netbird", "p1"}, {"netbird", "p2"}}},
		{"*prod", []AssetIdentity{{"netbird", "p1"}}}, // matches either name
		{"example.com", []AssetIdentity{{"cloudflare", "z1"}}},
		{"*", []AssetIdentity{{"cloudflare", "z1"}, {"netbird", "p1"}, {"netbird", "p2"}}},
		{"nothing-here", nil},
		{"  ", nil},
	}
	for _, tc := range cases {
		got, err := st.MatchAssetIdentities(ctx, tc.selector)
		if err != nil {
			t.Fatalf("MatchAssetIdentities(%q): %v", tc.selector, err)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("MatchAssetIdentities(%q) = %v, want %v", tc.selector, got, tc.want)
		}
	}
}

func TestAssetTimeline_OrdersOldestFirstAndScopesToIdentity(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	first := seedSeries(t, st, 72*time.Hour, []string{"netbird"},
		peer("p1", "gw", "connected"), peer("p2", "other", "connected"))
	second := seedSeries(t, st, 48*time.Hour, []string{"netbird"},
		peer("p1", "gw", "disconnected"))
	third := seedSeries(t, st, 0, []string{"netbird"},
		peer("p1", "gw-renamed", "connected"))

	obs, err := st.AssetTimeline(ctx, []AssetIdentity{{Provider: "netbird", ID: "p1"}})
	if err != nil {
		t.Fatalf("AssetTimeline: %v", err)
	}
	if len(obs) != 3 {
		t.Fatalf("got %d observations, want 3: %+v", len(obs), obs)
	}
	wantAudits := []int64{first, second, third}
	for i, o := range obs {
		if o.AuditID != wantAudits[i] {
			t.Errorf("observation %d audit = %d, want %d (oldest first)", i, o.AuditID, wantAudits[i])
		}
		if o.Asset.ID != "p1" {
			t.Errorf("observation %d leaked asset %q", i, o.Asset.ID)
		}
		if o.RunAt.IsZero() {
			t.Errorf("observation %d has no run time", i)
		}
	}
	if obs[1].Asset.Status != "disconnected" || obs[2].Asset.Name != "gw-renamed" {
		t.Errorf("timeline did not carry per-snapshot field values: %+v", obs)
	}

	// A provider that never minted this id must not pull the rows in, even
	// though asset_id alone matches.
	other, err := st.AssetTimeline(ctx, []AssetIdentity{{Provider: "oci", ID: "p1"}})
	if err != nil || len(other) != 0 {
		t.Errorf("AssetTimeline(wrong provider) = (%d obs, %v), want empty", len(other), err)
	}
	if none, err := st.AssetTimeline(ctx, nil); err != nil || none != nil {
		t.Errorf("AssetTimeline(nil) = (%v, %v), want (nil, nil)", none, err)
	}
}

func TestAssetTimeline_OmitsRawAndKeepsTags(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	created := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	seedSeries(t, st, 0, []string{"netbird"}, core.Asset{
		Provider: "netbird", Type: "netbird.peer", ID: "p1", Name: "gw",
		Tags: map[string]string{"ip": "100.64.0.1"}, CreatedAt: &created,
		Raw: []byte(`{"secret":"no"}`),
	})

	obs, err := st.AssetTimeline(ctx, []AssetIdentity{{Provider: "netbird", ID: "p1"}})
	if err != nil || len(obs) != 1 {
		t.Fatalf("AssetTimeline = (%d, %v)", len(obs), err)
	}
	if obs[0].Asset.Tags["ip"] != "100.64.0.1" {
		t.Errorf("tags did not round-trip: %+v", obs[0].Asset.Tags)
	}
	if obs[0].Asset.CreatedAt == nil || !obs[0].Asset.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", obs[0].Asset.CreatedAt, created)
	}
	// Raw is deliberately never loaded: diff excludes it and it is the
	// widest column in the table.
	if obs[0].Asset.Raw != nil {
		t.Errorf("Raw was loaded (%s); the timeline must not pay for it", obs[0].Asset.Raw)
	}
}

func TestAssetTimeline_BatchesBeyondTheBindLimit(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	n := sqlBatch + 25
	assets := make([]core.Asset, 0, n)
	ids := make([]AssetIdentity, 0, n)
	for i := range n {
		id := "p" + itoa(i)
		assets = append(assets, peer(id, "peer", "connected"))
		ids = append(ids, AssetIdentity{Provider: "netbird", ID: id})
	}
	seedSeries(t, st, 0, []string{"netbird"}, assets...)

	obs, err := st.AssetTimeline(ctx, ids)
	if err != nil {
		t.Fatalf("AssetTimeline over %d ids: %v", n, err)
	}
	if len(obs) != n {
		t.Errorf("got %d observations across %d batched ids, want %d", len(obs), n, n)
	}
}

// A store can accumulate several independent histories. A caller that picks
// one series on the user's behalf needs to be able to say what it ignored, and
// that starts with being able to enumerate them.
func TestProviderSets_EnumeratesEachSeriesMostHistoryFirst(t *testing.T) {
	st := openWhiteBox(t)
	ctx := context.Background()

	full := []string{"oci", "cloudflare"} // deliberately unsorted on the way in
	seedSeries(t, st, 60*24*time.Hour, full, peer("p1", "gw", "connected"))
	seedSeries(t, st, 30*24*time.Hour, full, peer("p1", "gw", "connected"))
	seedSeries(t, st, 10*24*time.Hour, full, peer("p1", "gw", "connected"))
	seedSeries(t, st, 2*24*time.Hour, []string{"netbird"}, peer("p1", "gw", "connected"))

	sets, err := st.ProviderSets(ctx)
	if err != nil {
		t.Fatalf("ProviderSets: %v", err)
	}
	if len(sets) != 2 {
		t.Fatalf("got %d provider sets, want 2: %+v", len(sets), sets)
	}

	// Most history first, so a caller suggesting an alternative suggests the
	// series the user most likely meant.
	if got, want := sets[0].Providers, []string{"cloudflare", "oci"}; !reflect.DeepEqual(got, want) {
		t.Errorf("sets[0].Providers = %v, want %v (canonicalized: lower-cased and sorted)", got, want)
	}
	if sets[0].Count != 3 {
		t.Errorf("sets[0].Count = %d, want 3", sets[0].Count)
	}
	if sets[1].Count != 1 || !reflect.DeepEqual(sets[1].Providers, []string{"netbird"}) {
		t.Errorf("sets[1] = %+v, want the single netbird snapshot", sets[1])
	}

	// Newest is what an "and here is how recent it is" message reports.
	if age := time.Since(sets[0].Newest); age < 9*24*time.Hour || age > 11*24*time.Hour {
		t.Errorf("sets[0].Newest is %v old, want ~10d (the most recent of that series)", age)
	}

	// Key is how a caller compares the chosen set against the listed ones
	// without caring how either was spelled.
	if got, want := sets[0].Key(), "cloudflare,oci"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
	if sets[0].Key() == sets[1].Key() {
		t.Error("two distinct series must not share a key")
	}
}

func TestProviderSets_EmptyStore(t *testing.T) {
	sets, err := openWhiteBox(t).ProviderSets(context.Background())
	if err != nil {
		t.Fatalf("ProviderSets on an empty store: %v", err)
	}
	if len(sets) != 0 {
		t.Errorf("got %+v, want no sets", sets)
	}
}

// itoa avoids pulling strconv in for one call site in a test.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
