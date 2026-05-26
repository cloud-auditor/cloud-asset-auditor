// Package telemetry sets up OpenTelemetry tracing for the project.
//
// Tracing is opt-in (--tracing=off by default) so cold-start cost is zero
// for users who don't want it. When on, every Provider.Collect call is a
// child span of the parent "audit" span; the HTTP server emits one span
// per request (with /healthz filtered out as noise). Spans propagate via
// W3C TraceContext + Baggage so the trace can be stitched together with
// upstream and downstream services that follow the same spec.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	// Must match the semconv version resource.Default() uses; otherwise
	// resource.Merge fails with "conflicting Schema URL".
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// ServiceName is the service.name resource attribute the project's spans
// are tagged with. Exported so tests + the HTTP middleware can use the
// same string consistently.
const ServiceName = "cloud-asset-auditor"

// Options drives telemetry.Setup. Zero values are valid: Mode="off" (no
// tracing, no overhead).
type Options struct {
	Mode           string // "off" | "stdout" | "otlp"
	ServiceVersion string // baked into the resource via semconv.ServiceVersion
	OTLPEndpoint   string // explicit OTLP endpoint URL; empty honors OTEL_EXPORTER_OTLP_*
}

var (
	mu         sync.Mutex
	shutdownFn func(context.Context) error
)

// Setup installs the global TracerProvider + TextMapPropagator. Idempotent
// (a second call shuts down the previous provider first), so callers don't
// have to coordinate.
//
// Returns immediately on Mode="off": the noop provider is installed so
// `telemetry.Tracer().Start(...)` is a free no-op everywhere.
func Setup(ctx context.Context, opts Options) error {
	mu.Lock()
	defer mu.Unlock()

	// Shut any previous provider down first so re-init is safe.
	if shutdownFn != nil {
		_ = shutdownFn(ctx)
		shutdownFn = nil
	}

	mode := strings.ToLower(strings.TrimSpace(opts.Mode))
	if mode == "" || mode == "off" {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return nil
	}

	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(ServiceName),
			semconv.ServiceVersion(opts.ServiceVersion),
		),
	)
	if err != nil {
		return fmt.Errorf("telemetry: build resource: %w", err)
	}

	exporter, err := buildExporter(ctx, mode, opts)
	if err != nil {
		return err
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	shutdownFn = tp.Shutdown
	return nil
}

// Shutdown flushes any pending spans. Safe to call when Setup was never
// invoked (no-op).
func Shutdown(ctx context.Context) error {
	mu.Lock()
	fn := shutdownFn
	shutdownFn = nil
	mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn(ctx)
}

// Tracer returns the project's named tracer. Safe to call before Setup —
// the global provider defaults to noop until Setup installs a real one.
func Tracer() trace.Tracer {
	return otel.Tracer(ServiceName)
}

func buildExporter(ctx context.Context, mode string, opts Options) (sdktrace.SpanExporter, error) {
	switch mode {
	case "stdout":
		// Write to stderr, not stdouttrace's default os.Stdout, because
		// stdout is reserved for renderer output (init-plan.md §6 invariant
		// 3). Without this override `auditor audit -o json --tracing stdout
		// | jq` would interleave asset JSON with span JSON and jq blows up.
		// PrettyPrint is the dev-mode default — verbose, but you only turn
		// this on locally.
		exp, err := stdouttrace.New(
			stdouttrace.WithWriter(os.Stderr),
			stdouttrace.WithPrettyPrint(),
		)
		if err != nil {
			return nil, fmt.Errorf("telemetry: stdout exporter: %w", err)
		}
		return exp, nil
	case "otlp":
		// HTTP not gRPC — pulls less weight. Endpoint precedence:
		// explicit Options.OTLPEndpoint > OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
		// > OTEL_EXPORTER_OTLP_ENDPOINT (the OTel SDK handles the
		// fallthrough automatically when we don't pass WithEndpoint).
		httpOpts := []otlptracehttp.Option{}
		if opts.OTLPEndpoint != "" {
			httpOpts = append(httpOpts, otlptracehttp.WithEndpointURL(opts.OTLPEndpoint))
		}
		exp, err := otlptracehttp.New(ctx, httpOpts...)
		if err != nil {
			return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
		}
		return exp, nil
	default:
		return nil, fmt.Errorf("telemetry: unknown tracing mode %q (want off|stdout|otlp)", mode)
	}
}

// ErrShutdown is returned by Shutdown when the underlying provider's
// shutdown reports a non-nil error. Exported so callers can errors.Is
// without depending on the SDK's error types.
var ErrShutdown = errors.New("telemetry shutdown")
