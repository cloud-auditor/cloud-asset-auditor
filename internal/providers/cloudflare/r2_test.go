package cloudflare

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/cloudflare/cloudflare-go/v4/r2"
)

func TestR2BucketToAsset_BasicMapping(t *testing.T) {
	p := &Provider{}
	b := r2.Bucket{
		Name:         "my-bucket",
		CreationDate: "2022-06-24T19:58:49.477Z",
		Location:     r2.BucketLocationWeur,
		StorageClass: r2.BucketStorageClassStandard,
		Jurisdiction: r2.BucketJurisdictionEu,
	}

	a := p.r2BucketToAsset("acct-r2-1", b)

	if a.Provider != "cloudflare" {
		t.Errorf("Provider = %q, want cloudflare", a.Provider)
	}
	if a.Type != "cloudflare.r2_bucket" {
		t.Errorf("Type = %q, want cloudflare.r2_bucket", a.Type)
	}
	if a.AccountID != "acct-r2-1" {
		t.Errorf("AccountID = %q, want acct-r2-1", a.AccountID)
	}
	if a.ID != "acct-r2-1/my-bucket" {
		t.Errorf("ID = %q, want acct-r2-1/my-bucket (composed account/name)", a.ID)
	}
	if a.Name != "my-bucket" {
		t.Errorf("Name = %q, want my-bucket", a.Name)
	}
	if a.Status != "" {
		t.Errorf("Status = %q, want empty (buckets have no status)", a.Status)
	}
	want := time.Date(2022, 6, 24, 19, 58, 49, 477000000, time.UTC)
	if a.CreatedAt == nil || !a.CreatedAt.Equal(want) {
		t.Errorf("CreatedAt = %v, want %v", a.CreatedAt, want)
	}
	if a.Tags["location"] != "weur" {
		t.Errorf("Tags[location] = %q, want weur", a.Tags["location"])
	}
	if a.Tags["storage_class"] != "Standard" {
		t.Errorf("Tags[storage_class] = %q, want Standard", a.Tags["storage_class"])
	}
	if a.Tags["jurisdiction"] != "eu" {
		t.Errorf("Tags[jurisdiction] = %q, want eu", a.Tags["jurisdiction"])
	}
	if a.Raw != nil {
		t.Errorf("Raw should be nil when IncludeRaw=false, got %s", a.Raw)
	}
}

func TestR2BucketToAsset_EmptyOptionalFieldsOmitTags(t *testing.T) {
	p := &Provider{}
	b := r2.Bucket{Name: "bare-bucket"}

	a := p.r2BucketToAsset("acct-r2-2", b)

	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil for empty creation_date, got %v", a.CreatedAt)
	}
	for _, k := range []string{"location", "storage_class", "jurisdiction"} {
		if v, ok := a.Tags[k]; ok {
			t.Errorf("Tags[%s] should be absent when SDK field is empty, got %q", k, v)
		}
	}
}

func TestR2BucketToAsset_UnparsableCreationDate(t *testing.T) {
	p := &Provider{}
	b := r2.Bucket{Name: "odd-bucket", CreationDate: "not-a-timestamp"}

	a := p.r2BucketToAsset("acct-r2-3", b)

	if a.CreatedAt != nil {
		t.Errorf("CreatedAt should be nil on parse failure, got %v", a.CreatedAt)
	}
}

func TestR2BucketToAsset_IncludeRawRoundTrip(t *testing.T) {
	p := &Provider{cfg: Config{IncludeRaw: true}}
	b := r2.Bucket{
		Name:         "raw-bucket",
		CreationDate: "2023-01-15T08:30:00Z",
		Location:     r2.BucketLocationApac,
	}

	a := p.r2BucketToAsset("acct-r2-4", b)

	if a.Raw == nil {
		t.Fatal("Raw should be populated when IncludeRaw=true")
	}
	var back map[string]any
	if err := json.Unmarshal(a.Raw, &back); err != nil {
		t.Fatalf("Raw is not valid JSON: %v", err)
	}
	if back["name"] != "raw-bucket" {
		t.Errorf("Raw.name = %v, want raw-bucket", back["name"])
	}
	if back["location"] != "apac" {
		t.Errorf("Raw.location = %v, want apac", back["location"])
	}
}

func TestR2ParseCreationDate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want *time.Time
	}{
		{name: "empty", in: "", want: nil},
		{name: "garbage", in: "yesterday", want: nil},
		{
			name: "rfc3339 with fractional seconds",
			in:   "2022-06-24T19:58:49.477Z",
			want: r2TestTimePtr(time.Date(2022, 6, 24, 19, 58, 49, 477000000, time.UTC)),
		},
		{
			name: "rfc3339 without fraction",
			in:   "2024-12-01T00:00:00Z",
			want: r2TestTimePtr(time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC)),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r2ParseCreationDate(tc.in)
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("r2ParseCreationDate(%q) = %v, want nil", tc.in, got)
			case tc.want != nil && (got == nil || !got.Equal(*tc.want)):
				t.Errorf("r2ParseCreationDate(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func r2TestTimePtr(t time.Time) *time.Time { return &t }
