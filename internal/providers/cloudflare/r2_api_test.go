package cloudflare

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// bucketNames returns n lexicographically ordered names, "bkt-0000"... .
func bucketNames(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s-%04d", prefix, i)
	}
	return out
}

// TestCollectR2_PagesViaStartAfter is the R2 pagination contract.
//
// The v4 SDK has no auto-pager for R2 buckets and BucketService.List discards
// the envelope's result_info.cursor entirely, so collectR2 drives pagination
// itself: buckets come back ordered lexicographically by name, and the next
// page resumes from the LAST name of a full page. Drop or mis-seed that cursor
// and every bucket list longer than one page is silently truncated — no error,
// no warning, just a short inventory.
//
// Three things are asserted on the wire, all of them load-bearing:
//   - page 1 carries no start_after (a mis-seeded cursor would skip buckets),
//   - per_page is the explicit r2ListPageSize (see below),
//   - page 2's start_after is the LAST name of page 1, not the first.
func TestCollectR2_PagesViaStartAfter(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))

	page1 := bucketNames("bkt", r2ListPageSize) // a full page => there is more
	page2 := []string{"zzz-0", "zzz-1"}         // a short page => stop
	f.route("/r2/buckets", func(w http.ResponseWriter, r *http.Request, nth int) {
		switch nth {
		case 0:
			fmt.Fprint(w, r2BucketsJSON(page1...))
		case 1:
			fmt.Fprint(w, r2BucketsJSON(page2...))
		default:
			t.Errorf("R2 listed a 3rd page (start_after=%q); a short page must end pagination",
				r.URL.Query().Get("start_after"))
			fmt.Fprint(w, r2BucketsJSON())
		}
	})
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectR2)
	if err != nil {
		t.Fatalf("collectR2() = %v, want nil", err)
	}

	if want := len(page1) + len(page2); len(got) != want {
		t.Fatalf("got %d bucket assets, want %d — a dropped cursor truncates the bucket list", len(got), want)
	}

	qs := f.queries("/r2/buckets")
	if len(qs) != 2 {
		t.Fatalf("GET .../r2/buckets called %d times, want 2 (full page then short page); paths=%v", len(qs), f.paths())
	}
	if sa := qs[0].Get("start_after"); sa != "" {
		t.Errorf("page 1 start_after = %q, want empty (the first page must not skip buckets)", sa)
	}
	if got, want := qs[0].Get("per_page"), fmt.Sprint(r2ListPageSize); got != want {
		// Without an explicit per_page the API applies its own (smaller)
		// default; the loop's `len(buckets) < r2ListPageSize` check then reads
		// page 1 as short and stops after a fraction of the buckets.
		t.Errorf("per_page on the wire = %q, want %q (must match r2ListPageSize or the stop condition misfires)", got, want)
	}
	last := page1[len(page1)-1]
	if got := qs[1].Get("start_after"); got != last {
		t.Errorf("page 2 start_after = %q, want %q — the cursor is the LAST name of the previous page "+
			"(R2 orders buckets lexicographically), not the first (%q)", got, last, page1[0])
	}

	// The last asset emitted must come from page 2, proving the second page was
	// not just requested but actually consumed.
	if want := "acct-1/" + page2[len(page2)-1]; got[len(got)-1].ID != want {
		t.Errorf("last asset ID = %q, want %q", got[len(got)-1].ID, want)
	}
}

// TestCollectR2_ShortFirstPageMakesNoSecondCall is the cheap half of the same
// contract: the common case (a handful of buckets) must cost exactly one
// request. An off-by-one in the `< r2ListPageSize` check would double every
// account's R2 traffic.
func TestCollectR2_ShortFirstPageMakesNoSecondCall(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.static("/r2/buckets", r2BucketsJSON("a", "b", "c"))
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectR2)
	if err != nil {
		t.Fatalf("collectR2() = %v, want nil", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d assets, want 3", len(got))
	}
	if n := f.hits("/r2/buckets"); n != 1 {
		t.Errorf("GET .../r2/buckets called %d times for a 3-bucket account, want 1", n)
	}
}

// TestCollectR2_PaginatesEachAccountIndependently guards against the cursor
// leaking across accounts. params is reused across the account loop, so a
// start_after left over from account 1 would silently skip every bucket in
// account 2 whose name sorts before it.
func TestCollectR2_PaginatesEachAccountIndependently(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1", "acct-2"))

	// acct-1 has a full page followed by a short one; acct-2 has three buckets
	// whose names all sort BEFORE acct-1's cursor.
	full := bucketNames("mmm", r2ListPageSize)
	f.route("/r2/buckets", func(w http.ResponseWriter, r *http.Request, _ int) {
		switch {
		case strings.Contains(r.URL.Path, "acct-2"):
			fmt.Fprint(w, r2BucketsJSON("aaa-1", "aaa-2", "aaa-3"))
		case r.URL.Query().Get("start_after") != "":
			fmt.Fprint(w, r2BucketsJSON("nnn-1"))
		default:
			fmt.Fprint(w, r2BucketsJSON(full...))
		}
	})
	p := f.provider(t, Config{})

	got, err := collectVia(t, p.collectR2)
	if err != nil {
		t.Fatalf("collectR2() = %v, want nil", err)
	}
	if want := r2ListPageSize + 1 + 3; len(got) != want {
		t.Fatalf("got %d assets, want %d (%d + 1 for acct-1, 3 for acct-2)", len(got), want, r2ListPageSize)
	}

	byAccount := map[string][]url.Values{}
	f.mu.Lock()
	for _, c := range f.seen {
		if strings.HasSuffix(c.Path, "/r2/buckets") {
			parts := strings.Split(strings.Trim(c.Path, "/"), "/")
			byAccount[parts[1]] = append(byAccount[parts[1]], c.Query)
		}
	}
	f.mu.Unlock()

	if n := len(byAccount["acct-1"]); n != 2 {
		t.Errorf("acct-1 made %d R2 requests, want 2", n)
	}
	if n := len(byAccount["acct-2"]); n != 1 {
		t.Errorf("acct-2 made %d R2 requests, want 1", n)
	}
	if sa := byAccount["acct-2"][0].Get("start_after"); sa != "" {
		t.Errorf("acct-2's first R2 request carried start_after=%q; the cursor must reset per account "+
			"or every bucket sorting before it is silently dropped", sa)
	}
	// And the acct-2 buckets really are in the output.
	ids := strings.Join(assetIDs(got), " ")
	if !strings.Contains(ids, "acct-2/aaa-1") {
		t.Errorf("acct-2's buckets are missing from the output: %s", ids)
	}
}

// TestCollectR2_ErrorIsWrappedAndStopsThatAccount checks the failure path: a
// mid-pagination error must surface (not be swallowed into a short list).
func TestCollectR2_ErrorIsWrappedAndStopsThatAccount(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.fail("/r2/buckets", 403, 10042, "Please enable R2 through the Cloudflare Dashboard.")
	p := f.provider(t, Config{})

	_, err := collectVia(t, p.collectR2)
	if err == nil {
		t.Fatal("collectR2() = nil, want the upstream 403 to surface to the caller")
	}
	if !strings.Contains(err.Error(), "list r2 buckets") {
		t.Errorf("error %q should be wrapped with %q for provenance", err, "list r2 buckets")
	}
	// run() is what decides this particular error is benign; the collector
	// itself must not make that call. (See TestRun_DisabledServiceIsDropped.)
	if !looksLikeServiceDisabled(err) {
		t.Errorf("a real SDK 403/10042 must still be recognised as a disabled service by "+
			"looksLikeServiceDisabled; got %q", err)
	}
}
