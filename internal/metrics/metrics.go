// Package metrics exposes Prometheus instrumentation for the auditor.
//
// The package owns its own Registry rather than using promauto's global
// default — that keeps every metric this binary exposes intentional and
// prevents libraries we depend on from leaking their default metrics
// (process_* aside) into our /metrics endpoint.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Project-scoped registry. Exported so tests can introspect without
// scraping the HTTP endpoint.
var registry = prometheus.NewRegistry()

// Standard process + Go runtime collectors. Without these the endpoint
// would lack the universally-expected process_cpu_seconds_total /
// go_goroutines / go_gc_* series that every Prometheus dashboard expects.
func init() {
	registry.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
	)
}

// AssetsCollectedTotal counts every Asset successfully emitted, keyed by
// (provider, type) so dashboards can answer "how many DNS records?" or
// "how many compute instances?" at a glance.
var AssetsCollectedTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "auditor",
		Subsystem: "assets",
		Name:      "collected_total",
		Help:      "Total number of assets emitted by providers, partitioned by provider name and asset type.",
	},
	[]string{"provider", "type"},
)

// AuditDurationSeconds is a histogram of full-audit durations per
// provider. Buckets are intentionally wide — audit runs vary from
// sub-second (Cloudflare empty zone) to minutes (large K8s clusters).
var AuditDurationSeconds = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		Namespace: "auditor",
		Subsystem: "audit",
		Name:      "duration_seconds",
		Help:      "Distribution of per-provider audit durations.",
		Buckets:   []float64{0.1, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600},
	},
	[]string{"provider"},
)

// AuditErrorsTotal counts per-resource errors emitted by providers
// during a Collect run. Mirrors the auditor's exit code 2 (partial
// failure) — a non-zero rate is interesting before users hit the gate.
var AuditErrorsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "auditor",
		Subsystem: "audit",
		Name:      "errors_total",
		Help:      "Per-provider error count from Collect (each Forbidden / timeout / partial-result error).",
	},
	[]string{"provider"},
)

// SSEClients is the number of currently-attached `/api/v1/audit` SSE
// subscribers. Long audits keep this up; spikes signal browsers hung in
// the table-rendering path.
var SSEClients = prometheus.NewGauge(
	prometheus.GaugeOpts{
		Namespace: "auditor",
		Subsystem: "server",
		Name:      "sse_clients",
		Help:      "Number of active SSE connections currently streaming audit results.",
	},
)

func init() {
	registry.MustRegister(
		AssetsCollectedTotal,
		AuditDurationSeconds,
		AuditErrorsTotal,
		SSEClients,
	)
}

// Handler returns an http.Handler that serves the OpenMetrics text
// exposition of every metric the project tracks. Wire into the server's
// mux as GET /metrics.
func Handler() http.Handler {
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
		// Mirror the standard promhttp behavior of returning 503 when
		// the collector itself errors (per Prometheus best practice —
		// stale scrapes are worse than a missed scrape).
		ErrorHandling: promhttp.HTTPErrorOnError,
	})
}

// Registry returns the project's underlying *prometheus.Registry. Mostly
// useful for tests; production callers should use Handler.
func Registry() *prometheus.Registry { return registry }
