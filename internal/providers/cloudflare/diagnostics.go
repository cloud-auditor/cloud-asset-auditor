package cloudflare

import (
	"errors"
	"fmt"
	"strings"
)

// noAccountsHint is emitted when the API token can see zero accounts. That is
// almost always an under-scoped token (the "Edit zone DNS" preset, say) rather
// than an account that genuinely owns nothing: GET /accounts returns success
// with an empty result, so every account-scoped collector (R2, KV, Workers,
// D1, Pages, Access, Tunnels, mTLS certs, account rulesets) finds nothing to
// list. Surfacing it turns a baffling "only DNS" result into an actionable
// message.
const noAccountsHint = "cloudflare: the API token can see 0 accounts — all " +
	"account-scoped resources (R2, KV, Workers, D1, Pages, Access, Tunnels, " +
	"mTLS certificates, account rulesets) were skipped. This usually means the " +
	"token is missing the 'Account.Account Settings:Read' scope (and the " +
	"per-resource account scopes); see docs/providers.md for the full scope list"

// withScopeHint appends a short, actionable note to Cloudflare API errors that
// look like authorization failures (HTTP 403, or Cloudflare error codes 10000
// / 9109). The token is valid — it just lacks the scope for that resource — so
// the useful guidance is "grant the matching Read scope", not the opaque
// upstream "Authentication error". Non-authorization errors pass through
// unchanged.
func withScopeHint(err error) error {
	if err == nil {
		return nil
	}
	if !looksLikeAuthzError(err) {
		return err
	}
	return fmt.Errorf("%w (token likely missing the matching Read scope — see docs/providers.md)", err)
}

// looksLikeServiceDisabled reports whether err is Cloudflare telling us a
// service simply isn't turned on for this account (or the plan doesn't include
// it) — as opposed to a permission gap (looksLikeAuthzError) or a genuine
// failure. The canonical case is R2: even a token *with* the R2 read scope
// gets 403 code 10042 "Please enable R2 through the Cloudflare Dashboard"
// until R2 is enabled in the dashboard. These aren't actionable as scope
// changes — the account just hasn't adopted the service — so collectors skip
// them silently rather than spamming the error channel.
func looksLikeServiceDisabled(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, sig := range []string{
		`"code":10042`,              // R2 not enabled for the account
		"please enable",             // generic "enable X through the dashboard"
		"plan level does not allow", // feature not included in the account's plan
		"is not enabled",
		"not entitled",
		"not subscribed",
	} {
		if strings.Contains(s, sig) {
			return true
		}
	}
	return false
}

// filterServiceDisabled strips "service not enabled" causes out of err. For a
// joined error (errors.Join — e.g. the certificates collector bundles its three
// families) it keeps only the parts that are NOT disabled-service signals, so a
// real scope gap joined alongside a disabled-service note still surfaces.
// Returns nil when every cause was a disabled-service signal, which tells the
// caller to emit nothing at all.
func filterServiceDisabled(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var kept []error
		for _, e := range joined.Unwrap() {
			if f := filterServiceDisabled(e); f != nil {
				kept = append(kept, f)
			}
		}
		return errors.Join(kept...) // nil when kept is empty
	}
	if looksLikeServiceDisabled(err) {
		return nil
	}
	return err
}

// looksLikeAuthzError reports whether err is a Cloudflare permission denial.
// It matches on the HTTP status and the two error codes the v4 API returns for
// scope gaps: 10000 ("Authentication error") and 9109 ("Unauthorized to access
// requested resource").
func looksLikeAuthzError(err error) bool {
	s := err.Error()
	return strings.Contains(s, "403") ||
		strings.Contains(s, `"code":10000`) ||
		strings.Contains(s, `"code":9109`)
}
