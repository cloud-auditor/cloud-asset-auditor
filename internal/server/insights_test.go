package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/insight"
)

// The POST verb is what these tests exercise, because it is the one the UI
// calls and the one that can be driven without credentials: GET runs a real
// audit, and a test that stubs the providers would be testing the stub.
func postInsights(t *testing.T, srv *httptest.Server, query, body string) (*http.Response, []byte) {
	t.Helper()
	res, err := http.Post(srv.URL+"/api/v1/insights"+query, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /api/v1/insights%s: %v", query, err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	out, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read insights response: %v", err)
	}
	return res, out
}

// The reach fixture is reused deliberately: it is the canonical cross-provider
// chain, it carries Raw on two assets, and it produces real edges — so an
// insight run over it exercises the graph rather than a flat list.
func TestInsights_PostReportsFindingsWithTheirCaveats(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, body := postInsights(t, srv, "", reachBody())
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", res.StatusCode, body)
	}

	var got insight.Report
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode insights response: %v\nbody: %s", err, body)
	}
	if got.Disclaimer != insight.Disclaimer {
		t.Error("response dropped the disclaimer; it is a required field precisely so " +
			"findings cannot travel without the statement of what they are")
	}
	if got.Scope.Assets == 0 {
		t.Error("scope reports no assets, but the body carried some")
	}
	// The house rule, asserted on the wire rather than only in the framework:
	// nothing reaches a client without naming what it cannot know.
	for _, f := range got.Findings {
		if strings.TrimSpace(f.Caveat) == "" {
			t.Errorf("finding %q was published with no caveat", f.ID)
		}
		if strings.TrimSpace(f.Basis) == "" {
			t.Errorf("finding %q was published with no basis", f.ID)
		}
	}
	if len(got.Suppressed) > 0 {
		t.Errorf("server published a REFUSED list (%v) — that is a bug in an insight", got.Suppressed)
	}
}

// An unpriced server must not quietly omit the cost family. Skipped-with-a-
// reason is the whole point: an absent section reads as "nothing to report".
func TestInsights_CostFindingsAreSkippedNotSilent(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	_, body := postInsights(t, srv, "?only=cost.*", reachBody())

	var got insight.Report
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Findings) != 0 {
		t.Fatalf("got %d cost findings from a server started without --cost", len(got.Findings))
	}
	if len(got.Skipped) == 0 {
		t.Fatal("cost insights vanished instead of being reported as skipped")
	}
	for _, s := range got.Skipped {
		if !strings.Contains(s.Reason, "cost") {
			t.Errorf("skip reason %q does not say what would fix it", s.Reason)
		}
	}
}

func TestInsights_OnlySelectorMatchesIDAndFamily(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for _, selector := range []string{"?only=network", "?only=network.*", "?only=network,hygiene"} {
		_, body := postInsights(t, srv, selector, reachBody())
		var got insight.Report
		if err := json.Unmarshal(body, &got); err != nil {
			t.Fatalf("%s: decode: %v", selector, err)
		}
		for _, f := range got.Findings {
			if f.Family != "network" && f.Family != "hygiene" {
				t.Errorf("%s: leaked a %q finding", selector, f.Family)
			}
		}
	}
}

func TestInsights_HumanFormatsComeBackAsDownloads(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for format, wantType := range map[string]string{
		"table":    "text/plain",
		"markdown": "text/markdown",
	} {
		res, body := postInsights(t, srv, "?format="+format, reachBody())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: status = %d", format, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, wantType) {
			t.Errorf("%s: Content-Type = %q, want %s", format, ct, wantType)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Errorf("%s: Content-Disposition = %q, want an attachment", format, cd)
		}
		// The disclaimer travels with the human formats too — it is rendered
		// above the first finding, never appended as a footer.
		if !strings.Contains(string(body), "An inventory cannot see") {
			t.Errorf("%s: rendered report omitted the disclaimer", format)
		}
	}
}

// A bad parameter must fail before an audit runs, not truncate one after it.
func TestInsights_RejectsUnknownParams(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	for _, query := range []string{"?format=dot", "?severity=bogus"} {
		res, body := postInsights(t, srv, query, reachBody())
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400 (body: %s)", query, res.StatusCode, body)
		}
	}
}

func TestInsights_RejectsUndecodableBody(t *testing.T) {
	srv := httptest.NewServer(mustServer(t).Handler())
	defer srv.Close()

	res, _ := postInsights(t, srv, "", "not json")
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", res.StatusCode)
	}
}

func TestSplitCommaParams(t *testing.T) {
	got := splitCommaParams([]string{"exposure, hygiene.*", "", "cost"})
	want := []string{"exposure", "hygiene.*", "cost"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
