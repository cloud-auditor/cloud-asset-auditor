package cloudflare

import (
	"context"
	"errors"
	"strings"
	"testing"

	cf "github.com/cloudflare/cloudflare-go/v4"
)

// TestLooksLikeAuthzError_AgainstRealSDKErrors pins the scope-hint contract to
// the SDK's actual error format instead of hand-written strings.
//
// looksLikeAuthzError is pure substring matching over err.Error(). The existing
// unit test feeds it synthetic strings, which means a v4 upgrade that reformats
// *cloudflare.Error — dropping the status text, or no longer echoing the
// response body — would break scope detection in production while every test
// stayed green. Here the errors come out of a real SDK call.
func TestLooksLikeAuthzError_AgainstRealSDKErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		code   int
		msg    string
		want   bool
		// why: the signal this case is meant to exercise
		why string
	}{
		{
			name: "403 with code 10000", status: 403, code: 10000, msg: "Authentication error",
			want: true, why: "the canonical under-scoped-token response",
		},
		{
			name: "403 with code 9109", status: 403, code: 9109, msg: "Unauthorized to access requested resource",
			want: true, why: "the other scope-gap code the v4 API returns",
		},
		{
			name: "401 carrying code 10000", status: 401, code: 10000, msg: "Authentication error",
			want: true, why: "code matching must work without a 403 status in the string",
		},
		{
			name: "401 carrying code 9109", status: 401, code: 9109, msg: "Unauthorized",
			want: true, why: "same, for the second code",
		},
		{
			name: "429 rate limited", status: 429, code: 10013, msg: "More than 1200 requests per five minutes",
			want: false, why: "throttling is not a scope problem; suggesting a token change would mislead",
		},
		{
			name: "500 upstream failure", status: 500, code: 0, msg: "internal error",
			want: false, why: "a server fault is not a scope problem",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeAPI(t)
			f.fail("/zones", tc.status, tc.code, tc.msg)
			p := f.provider(t, Config{})

			_, err := p.listZones(context.Background())
			if err == nil {
				t.Fatalf("listZones() = nil, want an HTTP %d failure", tc.status)
			}

			var apiErr *cf.Error
			if !errors.As(err, &apiErr) {
				t.Fatalf("err is %T, want it to wrap *cloudflare.Error", err)
			}

			if got := looksLikeAuthzError(err); got != tc.want {
				t.Errorf("looksLikeAuthzError(<real SDK error>) = %v, want %v (%s)\n error: %s",
					got, tc.want, tc.why, err)
			}

			hinted := withScopeHint(err).Error()
			hasHint := strings.Contains(hinted, "missing the matching Read scope")
			if hasHint != tc.want {
				t.Errorf("withScopeHint added hint = %v, want %v (%s)", hasHint, tc.want, tc.why)
			}
		})
	}
}

// TestSDKErrorStillEmbedsTheResponseBody guards the assumption underneath
// looksLikeAuthzError and looksLikeServiceDisabled: both read Cloudflare error
// CODES out of the flattened error string, which only works while the SDK
// echoes the JSON body into Error(). If an upgrade stops doing that, every
// code-based classification (9109, 10000, 10042) silently degrades to
// status-only matching — R2's "not enabled" noise starts being reported as a
// real failure, and 401-based scope gaps stop being explained at all.
func TestSDKErrorStillEmbedsTheResponseBody(t *testing.T) {
	f := newFakeAPI(t)
	f.fail("/zones", 403, 9109, "Unauthorized to access requested resource")
	p := f.provider(t, Config{})

	_, err := p.listZones(context.Background())
	if err == nil {
		t.Fatal("listZones() = nil, want a 403")
	}
	if !strings.Contains(err.Error(), `"code":9109`) {
		t.Errorf("the SDK error no longer contains the raw response body, so code matching in "+
			"diagnostics.go is broken.\n got: %s", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("the SDK error no longer contains the HTTP status, so status matching in "+
			"diagnostics.go is broken.\n got: %s", err)
	}
}

// TestLooksLikeServiceDisabled_AgainstRealR2Error is the same pinning for the
// disabled-service path. R2 returns 403 code 10042 to accounts that never
// enabled R2 — including accounts whose token DOES carry the R2 read scope — so
// misclassifying it as a scope gap produces a wrong, unactionable instruction on
// every audit of every account without R2.
func TestLooksLikeServiceDisabled_AgainstRealR2Error(t *testing.T) {
	f := newFakeAPI(t)
	f.static("/accounts", accountsJSON("acct-1"))
	f.fail("/r2/buckets", 403, 10042, "Please enable R2 through the Cloudflare Dashboard.")
	p := f.provider(t, Config{})

	_, err := collectVia(t, p.collectR2)
	if err == nil {
		t.Fatal("collectR2() = nil, want the 403")
	}
	if !looksLikeServiceDisabled(err) {
		t.Errorf("a real R2 403/10042 was not recognised as a disabled service: %s", err)
	}
	if filterServiceDisabled(err) != nil {
		t.Errorf("a lone disabled-service error must filter down to nil, got %v", filterServiceDisabled(err))
	}
	// It also matches looksLikeAuthzError (it IS a 403) — which is exactly why
	// run() consults filterServiceDisabled FIRST. Pin the ordering dependency.
	if !looksLikeAuthzError(err) {
		t.Error("precondition changed: the R2 403 no longer looks like an authz error, so the " +
			"filterServiceDisabled-before-withScopeHint ordering in run() may no longer be load-bearing")
	}
}
