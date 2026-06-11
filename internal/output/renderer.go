// Package output renders streams of core.Asset values into one of the
// supported formats (JSON array, NDJSON, CSV, XLSX, HTML). JSON and CSV
// drain the input channel incrementally so memory stays bounded regardless
// of inventory size; XLSX and HTML are the two documented exceptions that
// must buffer the full set first (see their type comments for why).
package output

import (
	"context"
	"io"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Renderer writes a stream of assets to w. Implementations MUST consume the
// channel incrementally (no full buffering) so audits against very large
// inventories don't blow up memory; XLSX and HTML are the two sanctioned
// exceptions, buffering because their formats need the full set.
type Renderer interface {
	Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error
}
