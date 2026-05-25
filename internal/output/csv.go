package output

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// CSV renders assets as RFC 4180 CSV. Tags collapse into one column as
// "k1=v1;k2=v2" with keys sorted so output is deterministic — critical for
// diffs in CI and golden-file tests.
type CSV struct{}

var csvHeader = []string{
	"provider", "account_id", "region", "type",
	"id", "name", "status", "created_at", "tags",
}

func (r *CSV) Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error {
	cw := csv.NewWriter(w)
	defer cw.Flush() // best-effort on panic; natural-exit paths flush + check below.

	if err := cw.Write(csvHeader); err != nil {
		return fmt.Errorf("write header: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			cw.Flush()
			return ctx.Err()
		case a, ok := <-in:
			if !ok {
				cw.Flush()
				return cw.Error()
			}
			row := []string{
				a.Provider,
				a.AccountID,
				a.Region,
				a.Type,
				a.ID,
				a.Name,
				a.Status,
				formatTime(a.CreatedAt),
				flattenTags(a.Tags),
			}
			if err := cw.Write(row); err != nil {
				return fmt.Errorf("write row: %w", err)
			}
		}
	}
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func flattenTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+tags[k])
	}
	return strings.Join(parts, ";")
}
