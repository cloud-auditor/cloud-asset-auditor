package output

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestSanitizeSheetName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"production", "production"},
		{"a/b:c*d?e[f]g\\h", "a_b_c_d_e_f_g_h"},
		{strings.Repeat("x", 40), strings.Repeat("x", 31)},
		{"'quoted'", "quoted"},
		{"", "Sheet"},
		{"  spaced  ", "spaced"},
	}
	for _, c := range cases {
		used := map[string]bool{}
		if got := sanitizeSheetName(c.in, used); got != c.want {
			t.Errorf("sanitizeSheetName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSanitizeSheetName_Dedup(t *testing.T) {
	used := map[string]bool{}
	if got := sanitizeSheetName("dup", used); got != "dup" {
		t.Fatalf("first = %q", got)
	}
	if got := sanitizeSheetName("dup", used); got != "dup~1" {
		t.Errorf("second = %q, want dup~1", got)
	}
	if got := sanitizeSheetName("DUP", used); got != "DUP~2" {
		t.Errorf("third (case-insensitive collision) = %q, want DUP~2", got)
	}
	// Dedup of an over-long base keeps the result within 31 runes.
	long := strings.Repeat("y", 31)
	_ = sanitizeSheetName(long, used)
	second := sanitizeSheetName(long, used)
	if len([]rune(second)) > 31 {
		t.Errorf("deduped long name exceeds 31 runes: %q (%d)", second, len([]rune(second)))
	}
}

func TestPrettyHeader(t *testing.T) {
	cases := map[string]string{
		"cidr_block":     "Cidr Block",
		"compartment_id": "Compartment Id",
		"size_gb":        "Size Gb",
		"email":          "Email",
		"":               "",
	}
	for in, want := range cases {
		if got := prettyHeader(in); got != want {
			t.Errorf("prettyHeader(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnionTagKeys(t *testing.T) {
	assets := []core.Asset{
		{Tags: map[string]string{"compartment_id": "C", "shape": "E4"}},
		{Tags: map[string]string{"compartment_id": "C", "email": "a@x"}},
		{Tags: nil},
	}
	got := unionTagKeys(assets, map[string]bool{"compartment_id": true})
	want := []string{"email", "shape"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unionTagKeys = %v, want %v", got, want)
	}
}

func TestTruncRunes(t *testing.T) {
	if got := truncRunes("hello", 10); got != "hello" {
		t.Errorf("no-trunc = %q", got)
	}
	if got := truncRunes("hello", 3); got != "hel" {
		t.Errorf("ascii trunc = %q", got)
	}
	// Multibyte: 5 runes truncated to 3 must not split a rune.
	if got := truncRunes("héllo", 3); got != "hél" {
		t.Errorf("unicode trunc = %q", got)
	}
}
