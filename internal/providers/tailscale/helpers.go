package tailscale

import (
	"strconv"
	"strings"
	"time"
)

// parseTime parses an RFC3339 timestamp into the *time.Time Asset.CreatedAt
// expects, yielding nil for empty or unparseable values so a malformed field
// can't abort a mapper.
func parseTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return timePtr(t)
}

// tagsOf builds an Asset.Tags map from key,value pairs, dropping pairs whose
// value is empty so the output isn't a wall of blank tags. An odd trailing key
// is ignored. Returns nil (not an empty map) when nothing survives.
func tagsOf(kv ...string) map[string]string {
	out := make(map[string]string, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == "" {
			continue
		}
		out[kv[i]] = kv[i+1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func boolStr(b bool) string { return strconv.FormatBool(b) }

func intStr(n int) string { return strconv.Itoa(n) }

// joinStr comma-joins a string slice ("" for empty/nil).
func joinStr(ss []string) string { return strings.Join(ss, ",") }

// connectedStatus turns a device's control-plane connection flag into the
// human Status string the canonical Asset carries.
func connectedStatus(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}

// firstIPv4 returns the first 100.x.y.z Tailscale address from the device's
// address list, and firstIPv6 the first fd7a:-style one. Splitting them lets
// the topology index bucket a device under a v4 key (what DNS records and
// load balancers actually point at) without an IPv6 address shadowing it.
func firstIPv4(addrs []string) string {
	for _, a := range addrs {
		if !strings.Contains(a, ":") {
			return a
		}
	}
	return ""
}

func firstIPv6(addrs []string) string {
	for _, a := range addrs {
		if strings.Contains(a, ":") {
			return a
		}
	}
	return ""
}

// keyStatus derives a lifecycle string for an auth key. Tailscale reports
// revocation/expiry as timestamps plus an `invalid` flag rather than a single
// status field, so the precedence is spelled out here: explicitly revoked
// beats expired beats invalid-for-any-other-reason.
func keyStatus(k authKey, now time.Time) string {
	switch {
	case k.Revoked != "":
		return "revoked"
	case k.Expires != "" && expired(k.Expires, now):
		return "expired"
	case k.Invalid:
		return "invalid"
	default:
		return "active"
	}
}

// expired reports whether an RFC3339 timestamp is in the past. An
// unparseable timestamp is treated as not-expired: guessing "expired" on a
// parse failure would misreport a live key as dead.
func expired(ts string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return t.Before(now)
}
