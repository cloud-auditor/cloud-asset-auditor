package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// fiftyAccountIDs returns exactly accountsPageSize ids, i.e. a "full" page.
func fiftyAccountIDs(prefix string) []string {
	ids := make([]string, accountsPageSize)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s-%02d", prefix, i)
	}
	return ids
}

// TestListAccounts_StopsWhenTheAPIRepeatsTheLastPage pins the fix for a bug
// that already shipped once: GET /accounts, asked for a page past the last,
// REPEATS the final page instead of returning an empty result. The SDK's
// V4PagePaginationArray auto-pager only stops on an empty page, so it walked
// ~185 pages until Cloudflare rate-limited it and the audit appeared to hang.
//
// This fixture reproduces that server exactly (same 50 accounts for every
// page). Delete the "added == 0" stop condition in listAccounts and this test
// runs forever instead of passing.
func TestListAccounts_StopsWhenTheAPIRepeatsTheLastPage(t *testing.T) {
	f := newFakeAPI(t)
	page := accountsJSON(fiftyAccountIDs("dup")...)
	f.route("/accounts", func(w http.ResponseWriter, _ *http.Request, nth int) {
		// Break the loop after a few pages so a regression fails loudly here
		// instead of spinning until the go test timeout.
		if nth >= 4 {
			t.Errorf("GET /accounts reached page %d against a server that repeats the last page — "+
				"the repeat guard in listAccounts is gone", nth+1)
			fmt.Fprint(w, v4List(`[]`))
			return
		}
		fmt.Fprint(w, page)
	})
	p := f.provider(t, Config{})

	accts, err := p.listAccounts(context.Background())
	if err != nil {
		t.Fatalf("listAccounts() = %v, want nil", err)
	}
	if len(accts) != accountsPageSize {
		t.Errorf("got %d accounts, want %d (duplicates must be deduped, not accumulated)",
			len(accts), accountsPageSize)
	}
	// Two calls: page 1 collects 50, page 2 adds nothing new and stops.
	if n := f.hits("/accounts"); n != 2 {
		t.Errorf("GET /accounts called %d times, want 2 (the repeat guard must stop at page 2); paths=%v",
			n, f.paths())
	}
}

// TestListAccounts_FollowsPagesUntilAShortOne is the other half of the same
// contract: the repeat guard must not have made pagination itself a no-op.
// Three pages (50 + 50 + 10) must all be fetched and concatenated.
func TestListAccounts_FollowsPagesUntilAShortOne(t *testing.T) {
	f := newFakeAPI(t)
	pages := []string{
		accountsJSON(fiftyAccountIDs("a")...),
		accountsJSON(fiftyAccountIDs("b")...),
		accountsJSON("c-00", "c-01", "c-02"),
	}
	f.route("/accounts", func(w http.ResponseWriter, _ *http.Request, nth int) {
		if nth < len(pages) {
			fmt.Fprint(w, pages[nth])
			return
		}
		t.Errorf("GET /accounts requested page %d; the short 3rd page should have ended pagination", nth+1)
		fmt.Fprint(w, v4List(`[]`))
	})
	p := f.provider(t, Config{})

	accts, err := p.listAccounts(context.Background())
	if err != nil {
		t.Fatalf("listAccounts() = %v, want nil", err)
	}
	if len(accts) != 103 {
		t.Fatalf("got %d accounts, want 103 (50+50+3) — a dropped page silently halves the inventory", len(accts))
	}
	// Spot-check that page 3's accounts really made it through, not just a count.
	if accts[len(accts)-1].ID != "c-02" {
		t.Errorf("last account ID = %q, want c-02 (the final page's last item)", accts[len(accts)-1].ID)
	}
	if n := f.hits("/accounts"); n != 3 {
		t.Errorf("GET /accounts called %d times, want 3", n)
	}
}

// TestListAccounts_StopsOnAnEmptyPage covers a well-behaved API that returns a
// full page then an empty one — the stop condition the SDK's auto-pager relies
// on, which must keep working alongside the repeat guard.
func TestListAccounts_StopsOnAnEmptyPage(t *testing.T) {
	f := newFakeAPI(t)
	full := accountsJSON(fiftyAccountIDs("a")...)
	f.route("/accounts", func(w http.ResponseWriter, _ *http.Request, nth int) {
		if nth == 0 {
			fmt.Fprint(w, full)
			return
		}
		fmt.Fprint(w, v4List(`[]`))
	})
	p := f.provider(t, Config{})

	accts, err := p.listAccounts(context.Background())
	if err != nil {
		t.Fatalf("listAccounts() = %v, want nil", err)
	}
	if len(accts) != accountsPageSize {
		t.Errorf("got %d accounts, want %d", len(accts), accountsPageSize)
	}
	if n := f.hits("/accounts"); n != 2 {
		t.Errorf("GET /accounts called %d times, want 2", n)
	}
}

// TestListAccounts_SendsAnExplicitPerPage guards the precondition of the
// "short page means last page" stop condition. If per_page never reaches the
// server the API applies its own default (20 at the time of writing); page 1
// then looks short against accountsPageSize=50 and pagination stops after 20
// accounts, silently dropping every account-scoped resource beyond them.
func TestListAccounts_SendsAnExplicitPerPage(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("only-one"))
	p := f.provider(t, Config{})

	if _, err := p.listAccounts(context.Background()); err != nil {
		t.Fatalf("listAccounts() = %v, want nil", err)
	}
	qs := f.queries("/accounts")
	if len(qs) == 0 {
		t.Fatal("no request reached GET /accounts")
	}
	if got := qs[0].Get("per_page"); got != fmt.Sprint(accountsPageSize) {
		t.Errorf("per_page on the wire = %q, want %d (must match accountsPageSize)", got, accountsPageSize)
	}
	if got := qs[0].Get("page"); got != "1" {
		t.Errorf("page on the wire = %q, want 1", got)
	}
}

// TestListAccounts_ResolvesExactlyOnceUnderConcurrency pins the sync.Once
// cache. run() fires ten account-scoped collectors in parallel and every one of
// them calls listAccounts; without the Once that is ten identical round-trips
// (times pagination) on the endpoint most likely to be rate-limited.
func TestListAccounts_ResolvesExactlyOnceUnderConcurrency(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	p := f.provider(t, Config{})

	// The real fan-out: four different collectors, all needing the account list.
	collectors := []func(context.Context, chan<- core.Asset) error{
		p.collectAccounts,
		p.collectR2,
		p.collectKV,
		p.collectMTLSCertificates,
		p.collectAccessApps,
		p.collectAccountRulesets,
	}
	var wg sync.WaitGroup
	for _, fn := range collectors {
		wg.Add(1)
		go func(fn func(context.Context, chan<- core.Asset) error) {
			defer wg.Done()
			out := make(chan core.Asset, 64)
			if err := fn(context.Background(), out); err != nil {
				t.Errorf("collector returned %v, want nil", err)
			}
		}(fn)
	}
	wg.Wait()

	if n := f.hits("/accounts"); n != 1 {
		t.Errorf("GET /accounts called %d times across %d concurrent collectors, want 1 (sync.Once cache); paths=%v",
			n, len(collectors), f.paths())
	}
}

// TestListAccounts_ErrorIsStickyAndNotRetried pins accountsErr caching. A
// failed lookup must be remembered, not retried by each of the ten
// account-scoped collectors — an outage would otherwise turn into ten
// (times SDK retries) requests hammering an endpoint that is already unhappy.
func TestListAccounts_ErrorIsStickyAndNotRetried(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("/accounts", 500, 0, "internal error")
	p := f.provider(t, Config{})

	for i := 0; i < 4; i++ {
		accts, err := p.listAccounts(context.Background())
		if err == nil {
			t.Fatalf("call %d: listAccounts() = nil error, want the cached failure", i+1)
		}
		if !strings.Contains(err.Error(), "list accounts") {
			t.Errorf("call %d: error %q should be wrapped with %q for provenance", i+1, err, "list accounts")
		}
		if accts != nil {
			t.Errorf("call %d: accounts = %v, want nil alongside the error", i+1, accts)
		}
	}
	if n := f.hits("/accounts"); n != 1 {
		t.Errorf("GET /accounts called %d times over 4 listAccounts() calls, want 1 (the error must be cached)", n)
	}
}

// TestCollectAccounts_EmitsOneAssetPerAccount checks the account asset itself.
// Accounts double as grouping containers downstream (XLSX --sheet-by, topology),
// so ID and AccountID must both be the account id.
func TestCollectAccounts_EmitsOneAssetPerAccount(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1", "acct-2"))
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectAccounts)
	if err != nil {
		t.Fatalf("collectAccounts() = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d assets, want 2: %v", len(got), assetIDs(got))
	}
	for _, a := range got {
		if a.Type != "cloudflare.account" {
			t.Errorf("Type = %q, want cloudflare.account", a.Type)
		}
		if a.ID != a.AccountID {
			t.Errorf("ID = %q but AccountID = %q; an account asset must be keyed by its own id", a.ID, a.AccountID)
		}
		if a.CreatedAt == nil {
			t.Errorf("account %s: CreatedAt is nil, want the decoded created_on", a.ID)
		}
	}
}
