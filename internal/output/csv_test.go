package output_test

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/output"
)

func TestCSV_Populated(t *testing.T) {
	var buf bytes.Buffer
	if err := (&output.CSV{}).Render(context.Background(), feedAssets(fixtureAssets(t)), &buf); err != nil {
		t.Fatal(err)
	}
	assertGolden(t, "csv.golden", buf.Bytes())
}

func TestCSV_HeaderOnlyWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	ch := make(chan core.Asset)
	close(ch)
	if err := (&output.CSV{}).Render(context.Background(), ch, &buf); err != nil {
		t.Fatal(err)
	}
	want := "provider,account_id,region,type,id,name,status,created_at,tags\n"
	if got := buf.String(); got != want {
		t.Errorf("empty CSV = %q, want %q", got, want)
	}
}

func TestCSV_TagFlatteningIsDeterministic(t *testing.T) {
	// Build an asset with several tags inserted in non-alphabetical order to
	// expose any reliance on map iteration order.
	a := core.Asset{
		Provider: "x", AccountID: "y", Type: "t", ID: "i", Name: "n",
		Tags: map[string]string{"z": "1", "a": "2", "m": "3"},
	}
	ch := make(chan core.Asset, 1)
	ch <- a
	close(ch)

	var buf bytes.Buffer
	if err := (&output.CSV{}).Render(context.Background(), ch, &buf); err != nil {
		t.Fatal(err)
	}
	row := buf.String()
	if !strings.Contains(row, "a=2;m=3;z=1") {
		t.Errorf("tags not sorted; got row: %q", row)
	}
}
