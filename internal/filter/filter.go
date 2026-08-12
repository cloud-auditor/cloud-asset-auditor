// Package filter implements the --filter expression language the audit and
// topology commands use to narrow the asset stream at render time.
//
// Each expression is key=value[,value...] or key!=value[,value...]; keys are
// provider, account, region, type, id, name, status, or tag:KEY. Values are
// case-insensitive globs where * matches any run of characters (including
// none). Values within one expression OR together; separate expressions AND
// together. A negated expression passes when no value matches — including
// when the addressed tag is absent.
package filter

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// fieldKeys maps expression keys to asset-field selectors. "account" and
// "account_id" are aliases so expressions read naturally either way.
var fieldKeys = map[string]func(core.Asset) string{
	"provider":   func(a core.Asset) string { return a.Provider },
	"account":    func(a core.Asset) string { return a.AccountID },
	"account_id": func(a core.Asset) string { return a.AccountID },
	"region":     func(a core.Asset) string { return a.Region },
	"type":       func(a core.Asset) string { return a.Type },
	"id":         func(a core.Asset) string { return a.ID },
	"name":       func(a core.Asset) string { return a.Name },
	"status":     func(a core.Asset) string { return a.Status },
}

// clause is one parsed expression: a key, whether it addresses a tag, the OR
// set of glob patterns, and whether the sense is inverted.
type clause struct {
	key    string
	isTag  bool
	negate bool
	values []string
}

// Filter is a parsed set of expressions. The zero value and nil both match
// every asset.
type Filter struct {
	clauses []clause
}

// Parse builds a Filter from --filter expressions. An empty slice yields a
// filter that matches everything.
func Parse(exprs []string) (*Filter, error) {
	f := &Filter{}
	for _, expr := range exprs {
		c, err := parseClause(expr)
		if err != nil {
			return nil, err
		}
		f.clauses = append(f.clauses, c)
	}
	return f, nil
}

func parseClause(expr string) (clause, error) {
	eq := strings.Index(expr, "=")
	if eq <= 0 {
		return clause{}, fmt.Errorf("invalid filter %q (want key=value[,value...] or key!=value[,value...])", expr)
	}
	key, rawValues := expr[:eq], expr[eq+1:]
	c := clause{}
	if strings.HasSuffix(key, "!") {
		c.negate = true
		key = strings.TrimSuffix(key, "!")
	}
	key = strings.TrimSpace(key)
	if tagKey, ok := cutTagKey(key); ok {
		if tagKey == "" {
			return clause{}, fmt.Errorf("invalid filter %q: tag key is empty (want tag:KEY=value)", expr)
		}
		c.isTag = true
		c.key = tagKey
	} else {
		lower := strings.ToLower(key)
		if _, ok := fieldKeys[lower]; !ok {
			return clause{}, fmt.Errorf("invalid filter key %q (want %s, or tag:KEY)", key, strings.Join(validKeys(), "|"))
		}
		c.key = lower
	}
	for _, v := range strings.Split(rawValues, ",") {
		v = strings.TrimSpace(v)
		if v != "" {
			c.values = append(c.values, v)
		}
	}
	if len(c.values) == 0 {
		return clause{}, fmt.Errorf("invalid filter %q: no values", expr)
	}
	return c, nil
}

// cutTagKey strips a case-insensitive "tag:" prefix, preserving the case of
// the tag key itself (asset tag keys are case-sensitive).
func cutTagKey(key string) (string, bool) {
	if len(key) >= 4 && strings.EqualFold(key[:4], "tag:") {
		return key[4:], true
	}
	return "", false
}

func validKeys() []string {
	keys := make([]string, 0, len(fieldKeys))
	for k := range fieldKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Match reports whether the asset passes every clause. A nil or empty filter
// matches everything.
func (f *Filter) Match(a core.Asset) bool {
	if f == nil {
		return true
	}
	for _, c := range f.clauses {
		if !c.match(a) {
			return false
		}
	}
	return true
}

func (c clause) match(a core.Asset) bool {
	var value string
	if c.isTag {
		value = a.Tags[c.key]
	} else {
		value = fieldKeys[c.key](a)
	}
	matched := false
	for _, pattern := range c.values {
		if Glob(pattern, value) {
			matched = true
			break
		}
	}
	return matched != c.negate
}

// Chan returns a channel that forwards only matching assets from in, closing
// when in closes or ctx is done. A nil filter forwards everything.
func (f *Filter) Chan(ctx context.Context, in <-chan core.Asset) <-chan core.Asset {
	out := make(chan core.Asset)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case a, ok := <-in:
				if !ok {
					return
				}
				if !f.Match(a) {
					continue
				}
				select {
				case out <- a:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// Slice returns the matching subset of assets, preserving order. A nil or
// empty filter returns the input slice unchanged.
func (f *Filter) Slice(assets []core.Asset) []core.Asset {
	if f == nil || len(f.clauses) == 0 {
		return assets
	}
	out := make([]core.Asset, 0, len(assets))
	for _, a := range assets {
		if f.Match(a) {
			out = append(out, a)
		}
	}
	return out
}

// Empty reports whether the filter has no clauses (matches everything).
func (f *Filter) Empty() bool {
	return f == nil || len(f.clauses) == 0
}

// Glob reports whether value matches pattern, case-insensitively. * matches
// any run of characters (including none and path separators); every other
// character matches literally. Exported for reuse by internal/policy.
func Glob(pattern, value string) bool {
	p := strings.ToLower(pattern)
	v := strings.ToLower(value)
	if !strings.Contains(p, "*") {
		return p == v
	}
	parts := strings.Split(p, "*")
	if !strings.HasPrefix(v, parts[0]) {
		return false
	}
	v = v[len(parts[0]):]
	last := parts[len(parts)-1]
	for _, mid := range parts[1 : len(parts)-1] {
		if mid == "" {
			continue
		}
		i := strings.Index(v, mid)
		if i < 0 {
			return false
		}
		v = v[i+len(mid):]
	}
	return strings.HasSuffix(v, last)
}
