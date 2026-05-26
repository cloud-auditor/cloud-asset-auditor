package telemetry_test

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/telemetry"
)

func TestSetup_OffModeIsZeroOverhead(t *testing.T) {
	if err := telemetry.Setup(context.Background(), telemetry.Options{Mode: "off"}); err != nil {
		t.Fatalf("Setup(off) should be infallible; got %v", err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })

	// The noop tracer still hands out spans, but they're recording=false.
	_, span := telemetry.Tracer().Start(context.Background(), "smoke")
	defer span.End()
	if span.IsRecording() {
		t.Errorf("expected non-recording span in off mode")
	}
}

func TestSetup_EmptyModeBehavesAsOff(t *testing.T) {
	if err := telemetry.Setup(context.Background(), telemetry.Options{}); err != nil {
		t.Fatalf("Setup(zero opts) should fall back to off; got %v", err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })

	_, span := telemetry.Tracer().Start(context.Background(), "smoke")
	defer span.End()
	if span.IsRecording() {
		t.Errorf("empty mode should be treated as off")
	}
}

func TestSetup_StdoutMode(t *testing.T) {
	err := telemetry.Setup(context.Background(), telemetry.Options{
		Mode:           "stdout",
		ServiceVersion: "test-1.2.3",
	})
	if err != nil {
		t.Fatalf("Setup(stdout) failed: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = telemetry.Shutdown(ctx)
	})

	// Recording true under a real exporter.
	_, span := telemetry.Tracer().Start(context.Background(), "smoke",
		trace.WithAttributes(attribute.String("k", "v")),
	)
	if !span.IsRecording() {
		t.Errorf("expected recording span in stdout mode")
	}
	span.End()
}

func TestSetup_UnknownModeErrors(t *testing.T) {
	err := telemetry.Setup(context.Background(), telemetry.Options{Mode: "carrier-pigeon"})
	if err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

func TestShutdown_NoSetupIsHarmless(t *testing.T) {
	// Reset state — previous test may have left a provider installed.
	_ = telemetry.Shutdown(context.Background())

	if err := telemetry.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown without Setup must be a no-op; got %v", err)
	}
}

func TestSetup_Idempotent(t *testing.T) {
	// Two consecutive Setups must not leak the first provider —
	// the second's Setup is responsible for shutting the first down.
	if err := telemetry.Setup(context.Background(), telemetry.Options{Mode: "stdout"}); err != nil {
		t.Fatal(err)
	}
	if err := telemetry.Setup(context.Background(), telemetry.Options{Mode: "off"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = telemetry.Shutdown(context.Background()) })

	_, span := telemetry.Tracer().Start(context.Background(), "smoke")
	defer span.End()
	if span.IsRecording() {
		t.Errorf("expected non-recording after second Setup switched to off")
	}
}
