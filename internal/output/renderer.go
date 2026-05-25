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
