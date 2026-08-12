package gcp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// drain collects all assets and the first error from a Collect run.
func drain(assets <-chan core.Asset, errs <-chan error) ([]core.Asset, error) {
	var out []core.Asset
	for a := range assets {
		out = append(out, a)
	}
	var firstErr error
	for e := range errs {
		if firstErr == nil {
			firstErr = e
		}
	}
	return out, firstErr
}

func TestCollect_Paginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ":searchAllResources") {
			http.Error(w, "wrong path", http.StatusNotFound)
			return
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			_, _ = io.WriteString(w, `{"results":[{"name":"r1","assetType":"t","project":"projects/1"}],"nextPageToken":"PAGE2"}`)
		case "PAGE2":
			_, _ = io.WriteString(w, `{"results":[{"name":"r2","assetType":"t","project":"projects/1"}]}`)
		default:
			http.Error(w, "bad token", http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	p, err := New(Config{Scope: "projects/test", BaseURL: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	p.client.hc = srv.Client() // bypass ADC

	assets, errs := p.Collect(context.Background())
	got, cerr := drain(assets, errs)
	if cerr != nil {
		t.Fatalf("collect error: %v", cerr)
	}
	if len(got) != 2 || got[0].ID != "r1" || got[1].ID != "r2" {
		t.Errorf("pagination wrong, got %d assets: %+v", len(got), got)
	}
}

func TestCollect_APIErrorSurfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"cloudasset.assets.searchAllResources denied"}}`)
	}))
	defer srv.Close()

	p, _ := New(Config{Scope: "projects/test", BaseURL: srv.URL})
	p.client.hc = srv.Client()

	_, cerr := drain(p.Collect(context.Background()))
	if cerr == nil {
		t.Fatal("expected an error from a 403 response")
	}
	if !strings.Contains(cerr.Error(), "403") || !strings.Contains(cerr.Error(), "PERMISSION_DENIED") {
		t.Errorf("error should carry the API status, got: %v", cerr)
	}
}

func TestCollect_NoScopeIsSilentSkip(t *testing.T) {
	p, _ := New(Config{Scope: ""})
	got, cerr := drain(p.Collect(context.Background()))
	if len(got) != 0 || cerr != nil {
		t.Errorf("empty scope must be a silent no-op (so --gcp-* flags can enable it later); got %d assets, err=%v", len(got), cerr)
	}
}

func TestCollect_InvalidScopeErrors(t *testing.T) {
	// A scope with URL-significant characters must be rejected before it can
	// mangle the request URL, with a clear error.
	p, _ := New(Config{Scope: "projects/bad?inject=1"})
	got, cerr := drain(p.Collect(context.Background()))
	if len(got) != 0 {
		t.Errorf("expected no assets, got %d", len(got))
	}
	if cerr == nil || !strings.Contains(cerr.Error(), "invalid scope") {
		t.Errorf("expected an invalid-scope error, got: %v", cerr)
	}
}

// A server that returns the same nextPageToken forever must not loop forever —
// the no-progress guard stops it.
func TestCollect_NonAdvancingTokenTerminates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"results":[{"name":"r","assetType":"t","project":"projects/1"}],"nextPageToken":"STUCK"}`)
	}))
	defer srv.Close()

	p, _ := New(Config{Scope: "projects/test", BaseURL: srv.URL})
	p.client.hc = srv.Client()

	done := make(chan struct{})
	go func() {
		_, _ = drain(p.Collect(context.Background()))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Collect did not terminate on a non-advancing page token (infinite loop)")
	}
}

func TestNew_DefaultsConcurrency(t *testing.T) {
	p, _ := New(Config{Scope: "projects/x"})
	if p.cfg.MaxConcurrency != defaultMaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want %d", p.cfg.MaxConcurrency, defaultMaxConcurrency)
	}
}
