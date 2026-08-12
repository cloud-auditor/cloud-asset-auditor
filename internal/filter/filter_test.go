package filter

import (
	"context"
	"testing"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func testAsset() core.Asset {
	return core.Asset{
		Provider:  "oci",
		AccountID: "ocid1.tenancy.oc1..aaa",
		Region:    "eu-frankfurt-1",
		Type:      "oci.instance",
		ID:        "ocid1.instance.oc1..bbb",
		Name:      "web-01",
		Status:    "RUNNING",
		Tags:      map[string]string{"env": "prod", "Team": "platform"},
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []string{
		"provider",        // no =
		"=oci",            // empty key
		"bogus=x",         // unknown key
		"provider=",       // no values
		"provider=,,",     // only empty values
		"tag:=x",          // empty tag key
		"created_at=2024", // not a filterable field
	}
	for _, expr := range cases {
		if _, err := Parse([]string{expr}); err == nil {
			t.Errorf("Parse(%q): want error, got nil", expr)
		}
	}
}

func TestMatch_Fields(t *testing.T) {
	a := testAsset()
	cases := []struct {
		expr string
		want bool
	}{
		{"provider=oci", true},
		{"provider=OCI", true}, // case-insensitive values
		{"Provider=oci", true}, // case-insensitive keys
		{"provider=gcp", false},
		{"provider=gcp,oci", true}, // OR within one expression
		{"provider!=oci", false},
		{"provider!=gcp", true},
		{"account=ocid1.tenancy*", true},
		{"account_id=ocid1.tenancy*", true}, // alias
		{"region=eu-*", true},
		{"type=*.instance", true},
		{"type=oci.*", true},
		{"id=*bbb", true},
		{"name=web-??", false}, // ? is literal, not a wildcard
		{"name=web-*", true},
		{"status=running", true},
		{"status!=running", false},
	}
	for _, tc := range cases {
		f, err := Parse([]string{tc.expr})
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if got := f.Match(a); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestMatch_Tags(t *testing.T) {
	a := testAsset()
	cases := []struct {
		expr string
		want bool
	}{
		{"tag:env=prod", true},
		{"tag:env=dev", false},
		{"tag:env=dev,prod", true},
		{"tag:env!=dev", true},
		{"tag:env!=prod", false},
		{"tag:missing=x", false}, // absent tag never matches a positive clause
		{"tag:missing!=x", true}, // absent tag passes a negated clause
		{"tag:Team=platform", true},
		{"tag:team=platform", false}, // tag keys stay case-sensitive
		{"TAG:env=prod", true},       // the tag: prefix itself is case-insensitive
	}
	for _, tc := range cases {
		f, err := Parse([]string{tc.expr})
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.expr, err)
		}
		if got := f.Match(a); got != tc.want {
			t.Errorf("Match(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestMatch_ClausesAND(t *testing.T) {
	a := testAsset()
	f, err := Parse([]string{"provider=oci", "tag:env=prod"})
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(a) {
		t.Error("both clauses hold: want match")
	}
	f, err = Parse([]string{"provider=oci", "tag:env=dev"})
	if err != nil {
		t.Fatal(err)
	}
	if f.Match(a) {
		t.Error("one clause fails: want no match")
	}
}

func TestMatch_NilAndEmpty(t *testing.T) {
	var f *Filter
	if !f.Match(testAsset()) {
		t.Error("nil filter must match everything")
	}
	if !f.Empty() {
		t.Error("nil filter must report Empty")
	}
	f, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Match(testAsset()) || !f.Empty() {
		t.Error("empty filter must match everything and report Empty")
	}
}

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"", "", true},
		{"", "x", false},
		{"*", "", true},
		{"*", "anything", true},
		{"abc", "abc", true},
		{"abc", "ABC", true},
		{"a*c", "abc", true},
		{"a*c", "ac", true},
		{"a*c", "abx", false},
		{"*.instance", "oci.instance", true},
		{"oci.*", "oci.instance", true},
		{"*b*b", "abb", true},
		{"*b*b", "ab", false},
		{"a*b*c", "abc", true},
		{"a*a", "aa", true},
		{"a*a", "a", false},
		{"*/v1/*", "apps/v1/deployment", true}, // * crosses path separators
	}
	for _, tc := range cases {
		if got := Glob(tc.pattern, tc.value); got != tc.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestChan_FiltersAndCloses(t *testing.T) {
	f, err := Parse([]string{"provider=oci"})
	if err != nil {
		t.Fatal(err)
	}
	in := make(chan core.Asset, 3)
	in <- core.Asset{Provider: "oci", ID: "keep"}
	in <- core.Asset{Provider: "gcp", ID: "drop"}
	in <- core.Asset{Provider: "oci", ID: "keep2"}
	close(in)

	var got []string
	for a := range f.Chan(context.Background(), in) {
		got = append(got, a.ID)
	}
	if len(got) != 2 || got[0] != "keep" || got[1] != "keep2" {
		t.Errorf("Chan forwarded %v, want [keep keep2]", got)
	}
}

func TestChan_CtxCancel(t *testing.T) {
	var f *Filter
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan core.Asset) // never written, never closed
	out := f.Chan(ctx, in)
	cancel()
	select {
	case _, ok := <-out:
		if ok {
			t.Error("expected closed channel after cancel")
		}
	case <-time.After(2 * time.Second):
		t.Error("Chan did not close after ctx cancel")
	}
}

func TestSlice(t *testing.T) {
	f, err := Parse([]string{"tag:env=prod"})
	if err != nil {
		t.Fatal(err)
	}
	assets := []core.Asset{
		{ID: "a", Tags: map[string]string{"env": "prod"}},
		{ID: "b", Tags: map[string]string{"env": "dev"}},
		{ID: "c", Tags: map[string]string{"env": "prod"}},
	}
	got := f.Slice(assets)
	if len(got) != 2 || got[0].ID != "a" || got[1].ID != "c" {
		t.Errorf("Slice = %v, want assets a and c", got)
	}

	var nilf *Filter
	if out := nilf.Slice(assets); len(out) != 3 {
		t.Errorf("nil filter Slice: got %d assets, want all 3", len(out))
	}
}
