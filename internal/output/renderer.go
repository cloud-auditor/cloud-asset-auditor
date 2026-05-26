// Package output renders streams of core.Asset values into one of the
// supported wire formats (JSON array, NDJSON, CSV). Each renderer drains
// the input channel incrementally so memory stays bounded regardless of
// inventory size.
package output

import (
	"context"
	"io"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// Renderer writes a stream of assets to w. Implementations MUST consume the
// channel incrementally (no full buffering) so audits against very large
// inventories don't blow up memory.
type Renderer interface {
	Render(ctx context.Context, in <-chan core.Asset, w io.Writer) error
}
