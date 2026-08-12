package cloudflare

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// TestSendAsset_ReportsCancellation is the unit-level half of invariant 2. Every
// collector's inner loop is `if !sendAsset(...) { return }`, so a sendAsset that
// blocked instead of noticing cancellation would wedge the whole audit on Ctrl+C.
func TestSendAsset_ReportsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out := make(chan core.Asset) // unbuffered, nobody reading
	if sendAsset(ctx, out, core.Asset{ID: "x"}) {
		t.Error("sendAsset() = true on a cancelled context with no reader, want false")
	}

	// The live path still delivers.
	got := make(chan core.Asset, 1)
	if !sendAsset(context.Background(), got, core.Asset{ID: "y"}) {
		t.Error("sendAsset() = false on a live context with a ready channel, want true")
	}
	if a := <-got; a.ID != "y" {
		t.Errorf("delivered asset ID = %q, want y", a.ID)
	}
}

// TestCollectR2_AbandonsAStalledSendOnCancel exercises the same thing through a
// real collector, mid-page, with the consumer stopped.
//
// This is the shape a real Ctrl+C takes: the CLI's renderer stops reading, the
// context is cancelled, and the collector is parked inside sendAsset with 999
// buckets still to go. It must return promptly rather than wait for a reader
// that will never come back.
func TestCollectR2_AbandonsAStalledSendOnCancel(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/r2/buckets", r2BucketsJSON(bucketNames("bkt", 200)...))
	p := f.provider(t, Config{})

	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan core.Asset) // unbuffered: the collector blocks after each send
	done := make(chan error, 1)
	go func() { done <- p.collectR2(ctx, out) }()

	// Take one asset so we know the collector is inside its send loop, then stop
	// reading and cancel.
	select {
	case <-out:
	case <-time.After(10 * time.Second):
		t.Fatal("collectR2 emitted nothing")
	}
	cancel()

	select {
	case err := <-done:
		// Cancellation is not a failure: the caller already knows it cancelled.
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Errorf("collectR2() = %v after cancel, want nil or a context error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("collectR2 never returned after cancel — a blocked send must abort, " +
			"or Ctrl+C hangs the CLI (invariant 2)")
	}
}

// TestCollectPageRules_NullResultEmitsNothing covers the one non-paginated
// endpoint in the provider. /zones/{id}/pagerules returns the whole list in a
// single call, so collectPageRules has no auto-pager to lean on and dereferences
// the SDK's *[]PageRule itself.
//
// Cloudflare answers `"result": null` (not `[]`) for some zone states. Today the
// SDK hands back a pointer to its zero-valued envelope field, so the pointer is
// non-nil and the explicit nil guard in collectPageRules is belt-and-braces —
// but that is an SDK implementation detail, and the guard is what stands between
// a null body and a panic that would take down the whole process, not just this
// collector. What this test pins is the observable contract: a null result
// yields no error, no panic and no phantom asset.
func TestCollectPageRules_NullResultEmitsNothing(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
	f.static("/pagerules", v4List(`null`))
	p := f.provider(t, Config{})

	zs, err := p.listZones(context.Background())
	if err != nil {
		t.Fatalf("listZones() = %v, want nil", err)
	}
	got, err := collectVia(t, func(ctx context.Context, out chan<- core.Asset) error {
		return p.collectPageRules(ctx, zs[0], out)
	})
	if err != nil {
		t.Fatalf("collectPageRules() = %v, want nil for a null result", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d assets from a null result, want 0: %v", len(got), assetIDs(got))
	}
}

// TestCollectRulesets_ErrorsAreScopeLabelled keeps the two ruleset collectors
// distinguishable in the error channel. They share an endpoint, an asset Type
// and a mapper; only the wrapping text tells an operator whether to grant the
// account-level or the zone-level Rulesets:Read scope.
func TestCollectRulesets_ErrorsAreScopeLabelled(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/zones", zonesJSON("zone-1", "example.com", "acct-1"))
	f.fail("/rulesets", 403, 9109, "Unauthorized to access requested resource")
	p := f.provider(t, Config{})

	_, accountErr := collectVia(t, p.collectAccountRulesets)
	if accountErr == nil || !strings.Contains(accountErr.Error(), "list account rulesets") {
		t.Errorf("collectAccountRulesets() = %v, want an error naming the account scope", accountErr)
	}

	zs, err := p.listZones(context.Background())
	if err != nil {
		t.Fatalf("listZones() = %v, want nil", err)
	}
	_, zoneErr := collectVia(t, func(ctx context.Context, out chan<- core.Asset) error {
		return p.collectZoneRulesets(ctx, zs[0], out)
	})
	if zoneErr == nil || !strings.Contains(zoneErr.Error(), "list zone rulesets") {
		t.Errorf("collectZoneRulesets() = %v, want an error naming the zone scope", zoneErr)
	}
}

// TestCollectRulesets_PaginatesViaCursor pins the one cursor-paginated endpoint
// in the provider. The SDK's CursorPagination follows result_info.cursor; if a
// fixture (or the API) omits it the listing silently ends, so assert the second
// page is both requested with the cursor and consumed.
func TestCollectRulesets_PaginatesViaCursor(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.route("/rulesets", func(w http.ResponseWriter, r *http.Request, nth int) {
		if nth == 0 {
			_, _ = w.Write([]byte(v4ListCursor(
				`[{"id":"rs-1","name":"first","kind":"managed","phase":"http_request_firewall_managed","version":"1"}]`,
				"cursor-2")))
			return
		}
		if got := r.URL.Query().Get("cursor"); got != "cursor-2" {
			t.Errorf("page 2 cursor = %q, want cursor-2", got)
		}
		_, _ = w.Write([]byte(v4List(
			`[{"id":"rs-2","name":"second","kind":"managed","phase":"http_request_firewall_managed","version":"1"}]`)))
	})
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectAccountRulesets)
	if err != nil {
		t.Fatalf("collectAccountRulesets() = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rulesets, want 2 — the cursor was not followed: %v", len(got), assetIDs(got))
	}
	if got[1].ID != "rs-2" {
		t.Errorf("second asset ID = %q, want rs-2 (from page 2)", got[1].ID)
	}
}
