package netbird

import (
	"strconv"
	"strings"
	"time"
)

// parseTime parses an RFC3339 timestamp the NetBird API returns into the
// *time.Time Asset.CreatedAt expects, yielding nil for empty or unparseable
// values so a malformed field can't abort a mapper.
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

// enabledStatus / connectedStatus turn a boolean lifecycle flag into the
// human Status string the canonical Asset carries.
func enabledStatus(b bool) string {
	if b {
		return "enabled"
	}
	return "disabled"
}

func connectedStatus(b bool) string {
	if b {
		return "connected"
	}
	return "disconnected"
}

// joinStr comma-joins a string slice ("" for empty/nil).
func joinStr(ss []string) string { return strings.Join(ss, ",") }

// groupNames joins the names of group references (a readable tag value).
func groupNames(groups []groupRef) string {
	if len(groups) == 0 {
		return ""
	}
	names := make([]string, 0, len(groups))
	for _, g := range groups {
		names = append(names, g.Name)
	}
	return strings.Join(names, ",")
}

// groupIDs joins the ids of group references. Names are for humans; ids are
// what the topology layer joins on — group names are not unique and can be
// renamed, whereas the id is the stable handle a policy rule and a peer both
// point at.
func groupIDs(groups []groupRef) string {
	if len(groups) == 0 {
		return ""
	}
	ids := make([]string, 0, len(groups))
	for _, g := range groups {
		ids = append(ids, g.ID)
	}
	return strings.Join(ids, ",")
}
