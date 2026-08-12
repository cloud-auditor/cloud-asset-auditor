package cloudflare

import (
	"errors"
	"strings"
	"testing"
)

func TestLooksLikeAuthzError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"403 status", errors.New(`GET ".../load_balancers": 403 Forbidden`), true},
		{"code 10000", errors.New(`{"errors":[{"code":10000,"message":"Authentication error"}]}`), true},
		{"code 9109", errors.New(`{"errors":[{"code":9109,"message":"Unauthorized"}]}`), true},
		{"unrelated 5xx", errors.New(`500 Internal Server Error`), false},
		{"plain timeout", errors.New("context deadline exceeded"), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeAuthzError(c.err); got != c.want {
				t.Errorf("looksLikeAuthzError(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestLooksLikeServiceDisabled(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"r2 not enabled (10042)", errors.New(`403 Forbidden {"errors":[{"code":10042,"message":"Please enable R2 through the Cloudflare Dashboard."}]}`), true},
		{"plan level", errors.New(`400 {"errors":[{"code":1011,"message":"Plan level does not allow custom certificates with type "}]}`), true},
		{"generic please enable", errors.New("please enable this feature in the dashboard"), true},
		{"scope gap 9109 is NOT disabled", errors.New(`403 {"errors":[{"code":9109,"message":"Unauthorized to access requested resource"}]}`), false},
		{"auth error 10000 is NOT disabled", errors.New(`403 {"errors":[{"code":10000,"message":"Authentication error"}]}`), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := looksLikeServiceDisabled(c.err); got != c.want {
				t.Errorf("looksLikeServiceDisabled(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

func TestFilterServiceDisabled(t *testing.T) {
	disabled := errors.New(`{"errors":[{"code":10042,"message":"Please enable R2 through the Cloudflare Dashboard."}]}`)
	scopeGap := errors.New(`403 {"errors":[{"code":9109,"message":"Unauthorized"}]}`)

	if got := filterServiceDisabled(nil); got != nil {
		t.Errorf("nil should stay nil, got %v", got)
	}
	if got := filterServiceDisabled(disabled); got != nil {
		t.Errorf("a lone disabled-service error should be dropped to nil, got %v", got)
	}
	if got := filterServiceDisabled(scopeGap); got == nil {
		t.Error("a real scope-gap error must survive filtering")
	}

	// The certificates collector joins families with errors.Join: a real scope
	// gap bundled with a disabled-service note must still surface the scope gap.
	mixed := errors.Join(scopeGap, disabled)
	got := filterServiceDisabled(mixed)
	if got == nil {
		t.Fatal("mixed join lost the real error entirely")
	}
	if strings.Contains(got.Error(), "10042") {
		t.Errorf("disabled-service cause should have been stripped, got %q", got.Error())
	}
	if !strings.Contains(got.Error(), "9109") {
		t.Errorf("real scope-gap cause should remain, got %q", got.Error())
	}

	// All-disabled join collapses to nil (caller emits nothing).
	if got := filterServiceDisabled(errors.Join(disabled, disabled)); got != nil {
		t.Errorf("all-disabled join should collapse to nil, got %v", got)
	}
}

func TestWithScopeHint(t *testing.T) {
	if withScopeHint(nil) != nil {
		t.Error("withScopeHint(nil) should be nil")
	}

	// Authorization errors get the hint appended, and the original error is
	// still wrapped (errors.Is reaches it).
	orig := errors.New("403 Forbidden")
	got := withScopeHint(orig)
	if !strings.Contains(got.Error(), "missing the matching Read scope") {
		t.Errorf("hint not appended: %q", got.Error())
	}
	if !errors.Is(got, orig) {
		t.Error("withScopeHint must wrap the original error (errors.Is)")
	}

	// Non-authorization errors pass through with no hint appended.
	plain := errors.New("connection refused")
	if got := withScopeHint(plain).Error(); got != "connection refused" {
		t.Errorf("non-authz error should pass through unchanged, got %q", got)
	}
}

func TestNoAccountsHintMentionsScope(t *testing.T) {
	// The whole point of the message is to name the missing scope; guard it
	// so a future reword can't silently drop the actionable part.
	if !strings.Contains(noAccountsHint, "Account.Account Settings") {
		t.Errorf("noAccountsHint should name the Account.Account Settings scope: %q", noAccountsHint)
	}
	if !strings.Contains(noAccountsHint, "0 accounts") {
		t.Errorf("noAccountsHint should explain the zero-account symptom: %q", noAccountsHint)
	}
}
