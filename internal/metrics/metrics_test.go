package metrics_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/metrics"
)

func TestHandler_EmitsExpectedMetricNames(t *testing.T) {
	// Drive a few values through the collectors so they actually
	// surface in the exposition.
	metrics.AssetsCollectedTotal.WithLabelValues("cloudflare", "cloudflare.zone").Add(3)
	metrics.AuditDurationSeconds.WithLabelValues("oci").Observe(1.5)
	metrics.AuditErrorsTotal.WithLabelValues("kubernetes").Inc()
	metrics.SSEClients.Set(2)

	ts := httptest.NewServer(metrics.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	out := string(body)

	for _, want := range []string{
		"auditor_assets_collected_total",
		"auditor_audit_duration_seconds",
		"auditor_audit_errors_total",
		"auditor_server_sse_clients",
		// Standard process + go runtime collectors (auto-registered)
		"process_cpu_seconds_total",
		"go_goroutines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected metric %q in /metrics output, missing.\nGot first 500 bytes:\n%s",
				want, truncate(out, 500))
		}
	}
}

func TestCounter_AssetsCollectedTotal_Increments(t *testing.T) {
	// Reset by reading the current value and asserting the delta.
	before := testutil.ToFloat64(metrics.AssetsCollectedTotal.WithLabelValues("p", "t"))
	metrics.AssetsCollectedTotal.WithLabelValues("p", "t").Inc()
	after := testutil.ToFloat64(metrics.AssetsCollectedTotal.WithLabelValues("p", "t"))
	if after-before != 1 {
		t.Errorf("counter delta = %v, want 1", after-before)
	}
}

func TestGauge_SSEClients_TracksConnections(t *testing.T) {
	metrics.SSEClients.Set(0)
	metrics.SSEClients.Inc()
	metrics.SSEClients.Inc()
	metrics.SSEClients.Dec()
	if got := testutil.ToFloat64(metrics.SSEClients); got != 1 {
		t.Errorf("gauge = %v, want 1", got)
	}
}

func TestRegistry_ContentType(t *testing.T) {
	ts := httptest.NewServer(metrics.Handler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	// Prometheus exposition or OpenMetrics, depending on Accept negotiation.
	if !strings.HasPrefix(ct, "text/plain") && !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Errorf("Content-Type = %q, want text/plain or application/openmetrics-text", ct)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
