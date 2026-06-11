// Package diff compares two audit snapshots (the JSON produced by
// `auditor audit -o json`, array or NDJSON form) and reports drift:
// assets added, removed, or changed between the two runs.
//
// Identity: an asset is keyed by Provider + "|" + ID. Type is deliberately
// NOT part of the key — a provider could in theory reclassify a resource
// (same OCID, new type label) and that's far more useful surfaced as a
// "type" field change on one asset than as an unrelated remove + add pair.
//
// Comparison: only Name, Type, Region, AccountID, Status, and Tags are
// diffed. Raw is excluded because it's opt-in (--include-raw) noise — two
// snapshots taken with different flags would report every asset as changed
// — and its payload churns on fields nobody treats as drift (etags,
// timestamps). CreatedAt is excluded because it's immutable-ish: a creation
// time that "changes" almost always means the resource was deleted and
// recreated, which the ID change already surfaces as remove + add.
package diff

import (
	"bufio"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Result is the outcome of comparing two snapshots. All three slices are
// always non-nil so JSON output renders [] instead of null, and all are
// sorted by (provider, type, id) so two runs over the same inputs produce
// byte-identical output — same determinism contract as the topology
// renderers.
type Result struct {
	Added   []core.Asset `json:"added"`
	Removed []core.Asset `json:"removed"`
	Changed []Change     `json:"changed"`
}

// Empty reports whether the two snapshots matched (no drift at all).
func (r Result) Empty() bool {
	return len(r.Added) == 0 && len(r.Removed) == 0 && len(r.Changed) == 0
}

// Change is one asset present in both snapshots whose compared fields
// differ. Before/After carry the full assets so renderers (and API
// consumers) can show context beyond the changed fields.
type Change struct {
	Before core.Asset    `json:"before"`
	After  core.Asset    `json:"after"`
	Fields []FieldChange `json:"fields"`
}

// FieldChange records a single field-level difference. Field uses the
// asset's JSON names ("name", "status", …) so it matches what users see in
// audit output; tag differences are reported per key as "tags.<key>".
// Old/New are plain strings — a tag set to the empty string is therefore
// indistinguishable from an absent tag, which is acceptable: empty-string
// tag values don't occur in practice.
type FieldChange struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// Compute diffs two snapshots. Within one snapshot a duplicate
// (provider, id) key — which a well-behaved audit never produces — is
// resolved last-one-wins rather than erroring, so a slightly damaged
// snapshot still yields a useful report.
func Compute(oldAssets, newAssets []core.Asset) Result {
	oldByKey := keyed(oldAssets)
	newByKey := keyed(newAssets)

	res := Result{
		Added:   []core.Asset{},
		Removed: []core.Asset{},
		Changed: []Change{},
	}

	for k, after := range newByKey {
		before, ok := oldByKey[k]
		if !ok {
			res.Added = append(res.Added, after)
			continue
		}
		if fields := diffFields(before, after); len(fields) > 0 {
			res.Changed = append(res.Changed, Change{Before: before, After: after, Fields: fields})
		}
	}
	for k, before := range oldByKey {
		if _, ok := newByKey[k]; !ok {
			res.Removed = append(res.Removed, before)
		}
	}

	slices.SortFunc(res.Added, compareAssets)
	slices.SortFunc(res.Removed, compareAssets)
	// Changed sorts by the After side: provider and id are identical to
	// Before by construction, and on a type change the new snapshot's view
	// is the one the report leads with.
	slices.SortFunc(res.Changed, func(a, b Change) int { return compareAssets(a.After, b.After) })
	return res
}

// Key returns the identity key used to match assets across snapshots.
// Exported so callers presenting diff output can group by the same notion
// of identity.
func Key(a core.Asset) string { return a.Provider + "|" + a.ID }

func keyed(assets []core.Asset) map[string]core.Asset {
	m := make(map[string]core.Asset, len(assets))
	for _, a := range assets {
		m[Key(a)] = a
	}
	return m
}

func compareAssets(a, b core.Asset) int {
	if c := cmp.Compare(a.Provider, b.Provider); c != 0 {
		return c
	}
	if c := cmp.Compare(a.Type, b.Type); c != 0 {
		return c
	}
	return cmp.Compare(a.ID, b.ID)
}

// diffFields compares only the drift-relevant fields (see package doc for
// why Raw and CreatedAt are excluded). Scalar fields come first in a fixed
// order, then tags sorted by key, so the change list is deterministic.
func diffFields(before, after core.Asset) []FieldChange {
	var fields []FieldChange
	record := func(name, oldVal, newVal string) {
		if oldVal != newVal {
			fields = append(fields, FieldChange{Field: name, Old: oldVal, New: newVal})
		}
	}

	record("name", before.Name, after.Name)
	record("type", before.Type, after.Type)
	record("region", before.Region, after.Region)
	record("account_id", before.AccountID, after.AccountID)
	record("status", before.Status, after.Status)

	tagKeys := make(map[string]struct{}, len(before.Tags)+len(after.Tags))
	for k := range before.Tags {
		tagKeys[k] = struct{}{}
	}
	for k := range after.Tags {
		tagKeys[k] = struct{}{}
	}
	for _, k := range slices.Sorted(maps.Keys(tagKeys)) {
		record("tags."+k, before.Tags[k], after.Tags[k])
	}
	return fields
}

// Load reads one audit snapshot from r, accepting both shapes that
// `auditor audit -o json` produces: a JSON array (the default) and NDJSON
// (--stream). The shape is sniffed from the first non-whitespace byte —
// '[' means array, anything else is decoded as a stream of objects.
// Empty input is a valid zero-asset snapshot (exactly what --stream emits
// when no provider returns anything), not an error.
func Load(r io.Reader) ([]core.Asset, error) {
	br := bufio.NewReader(r)
	first, err := firstNonSpace(br)
	if errors.Is(err, io.EOF) {
		return []core.Asset{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read snapshot: %w", err)
	}

	dec := json.NewDecoder(br)
	if first == '[' {
		var assets []core.Asset
		if err := dec.Decode(&assets); err != nil {
			return nil, fmt.Errorf("decode JSON array: %w", err)
		}
		return assets, nil
	}

	assets := []core.Asset{}
	for {
		var a core.Asset
		if err := dec.Decode(&a); err != nil {
			if errors.Is(err, io.EOF) {
				return assets, nil
			}
			return nil, fmt.Errorf("decode NDJSON: %w", err)
		}
		assets = append(assets, a)
	}
}

// firstNonSpace peeks past JSON insignificant whitespace and reports the
// first content byte, leaving it unread for the decoder.
func firstNonSpace(br *bufio.Reader) (byte, error) {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return 0, err
		}
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			if err := br.UnreadByte(); err != nil {
				return 0, err
			}
			return b, nil
		}
	}
}
