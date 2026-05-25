package server_test

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/server"
)

func newTestServer(t *testing.T, cfg server.Config) *httptest.Server {
	t.Helper()
	s, err := server.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func TestHealthz_AlwaysOpen(t *testing.T) {
	// Even with auth=basic configured, /healthz must be reachable so
	// load balancers and probes work.
	ts := newTestServer(t, server.Config{
		AuthMode: "basic", BasicUser: "u", BasicPass: "p",
	})

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("got %d, want 200", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ok") {
		t.Errorf("body = %q, want contains 'ok'", b)
	}
}

func TestProviders_ReturnsRegistered(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var got struct {
		Providers []string `json:"providers"`
		AuthMode  string   `json:"auth_mode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	// The test runner can't predict which providers are registered (it
	// depends on which packages were imported), but the response shape
	// must be correct.
	if got.AuthMode != "none" {
		t.Errorf("auth_mode = %q, want none", got.AuthMode)
	}
	if got.Providers == nil {
		t.Error("providers field missing")
	}
}

func TestAuditSSE_NoProviders_EmitsDoneZero(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	// `providers=none` is the sentinel for "run zero providers".
	resp, err := http.Get(ts.URL + "/api/v1/audit?providers=none")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream*", ct)
	}

	events, err := readSSEUntilDone(resp.Body, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	gotEvents := map[string]int{}
	var doneData map[string]any
	for _, ev := range events {
		gotEvents[ev.name]++
		if ev.name == "done" {
			_ = json.Unmarshal([]byte(ev.data), &doneData)
		}
	}
	if gotEvents["meta"] != 1 {
		t.Errorf("want exactly 1 meta event, got %d", gotEvents["meta"])
	}
	if gotEvents["done"] != 1 {
		t.Errorf("want exactly 1 done event, got %d", gotEvents["done"])
	}
	if gotEvents["asset"] != 0 {
		t.Errorf("want 0 asset events for `providers=none`, got %d", gotEvents["asset"])
	}
	if got := doneData["count"]; got != float64(0) {
		t.Errorf("done.count = %v, want 0", got)
	}
}

func TestAuditExport_CSVHeaderOnly(t *testing.T) {
	ts := newTestServer(t, server.Config{})

	resp, err := http.Get(ts.URL + "/api/v1/audit/export?format=csv&providers=none")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "assets.csv") {
		t.Errorf("Content-Disposition = %q, want filename assets.csv", cd)
	}
	b, _ := io.ReadAll(resp.Body)
	got := string(b)
	want := "provider,account_id,region,type,id,name,status,created_at,tags\n"
	if got != want {
		t.Errorf("CSV body = %q, want %q", got, want)
	}
}

func TestAuditExport_UnknownFormat(t *testing.T) {
	ts := newTestServer(t, server.Config{})
	resp, err := http.Get(ts.URL + "/api/v1/audit/export?format=xml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestAuth_TokenMode_RejectsMissingHeader(t *testing.T) {
	ts := newTestServer(t, server.Config{AuthMode: "token", APIToken: "secret"})

	resp, err := http.Get(ts.URL + "/api/v1/providers")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
}

func TestAuth_TokenMode_AcceptsBearer(t *testing.T) {
	ts := newTestServer(t, server.Config{AuthMode: "token", APIToken: "secret"})
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.Header.Set("Authorization", "Bearer secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestAuth_BasicMode(t *testing.T) {
	ts := newTestServer(t, server.Config{
		AuthMode: "basic", BasicUser: "u", BasicPass: "p",
	})

	// No auth → 401.
	resp1, _ := http.Get(ts.URL + "/api/v1/providers")
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Errorf("no-auth status = %d, want 401", resp1.StatusCode)
	}

	// Wrong creds → 401.
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/providers", nil)
	req.SetBasicAuth("u", "wrong")
	resp2, _ := http.DefaultClient.Do(req)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong-pass status = %d, want 401", resp2.StatusCode)
	}

	// Correct creds → 200.
	req.SetBasicAuth("u", "p")
	resp3, _ := http.DefaultClient.Do(req)
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Errorf("good-auth status = %d, want 200", resp3.StatusCode)
	}
}

func TestNew_RejectsBadAuthConfig(t *testing.T) {
	if _, err := server.New(server.Config{AuthMode: "basic"}); err == nil {
		t.Error("expected error for basic without user/pass")
	}
	if _, err := server.New(server.Config{AuthMode: "token"}); err == nil {
		t.Error("expected error for token without API token")
	}
	if _, err := server.New(server.Config{AuthMode: "wat"}); err == nil {
		t.Error("expected error for unknown auth mode")
	}
}

// sseEvent is a parsed event from the SSE stream.
type sseEvent struct{ name, data string }

// readSSEUntilDone parses the SSE wire format until a `done` event is seen
// or the timeout elapses. Returns every event read in order.
func readSSEUntilDone(r io.Reader, timeout time.Duration) ([]sseEvent, error) {
	type result struct {
		events []sseEvent
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		var (
			out  []sseEvent
			name string
			data strings.Builder
		)
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 1<<16), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if name != "" {
					out = append(out, sseEvent{name: name, data: data.String()})
					if name == "done" {
						ch <- result{out, nil}
						return
					}
				}
				name = ""
				data.Reset()
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.WriteString(strings.TrimPrefix(line, "data: "))
			}
		}
		ch <- result{out, sc.Err()}
	}()

	select {
	case r := <-ch:
		return r.events, r.err
	case <-time.After(timeout):
		return nil, io.EOF
	}
}
