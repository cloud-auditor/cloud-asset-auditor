package output_test

import (
	"bytes"
	"context"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

func TestJSON_ArrayMode_Empty(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan core.Asset)
	close(ch)
	if err := (&output.JSON{}).Render(context.Background(), ch, &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "json_array_empty.golden", buf.Bytes())
}

func TestJSON_ArrayMode_Populated(t *testing.T) {
	var buf bytes.Buffer
	r := &output.JSON{}
	if err := r.Render(context.Background(), feedAssets(fixtureAssets(t)), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "json_array.golden", buf.Bytes())
}

func TestJSON_StreamMode(t *testing.T) {
	var buf bytes.Buffer
	r := &output.JSON{Stream: true}
	if err := r.Render(context.Background(), feedAssets(fixtureAssets(t)), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "json_stream.golden", buf.Bytes())
}

func TestJSON_StreamMode_Empty(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan core.Asset)
	close(ch)
	if err := (&output.JSON{Stream: true}).Render(context.Background(), ch, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 0 {
		t.Errorf("stream mode with no assets should emit nothing, got %q", buf.String())
	}
}

func TestJSON_ContextCancellation(t *testing.T) {
	// A consumer that never reads — render should bail out on ctx cancel
	// rather than block forever.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	src := make(chan core.Asset)
	// Never close src; rely on the ctx already being cancelled.
	err := (&output.JSON{}).Render(ctx, src, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
}
