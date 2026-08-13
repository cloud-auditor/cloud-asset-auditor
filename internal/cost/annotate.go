package cost

import (
	"context"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// StreamOnlyNotice is what `audit --cost` should say on stderr the first time
// it matters. It lives here so the limitation is described in one place
// alongside the code that has it.
const StreamOnlyNotice = "--cost: per-asset annotation only; Kubernetes pod attribution and per-seat " +
	"rollups need the full asset set — run `auditor cost` for those"

// Annotate stamps the cost.* tags onto each asset as it passes through.
//
// It is deliberately the same shape as filter.Chan: one goroutine, an
// unbuffered output channel, no lookahead and no accumulation, so memory stays
// O(1) against a 50k-object Kubernetes cluster and the web UI still renders
// rows as they arrive. Wire it into the audit pipeline in the same position as
// the --filter stage, and before it, so that `--filter tag:cost.basis=unknown`
// has something to match on.
//
// The output channel closes exactly once — on a deferred close — when in closes
// or ctx is done, whichever happens first. A cancelled context abandons the
// remaining input rather than draining it, matching every other stage in the
// pipeline: the provider goroutines are watching the same context and are
// stopping too.
//
// This is Stage A in full. It cannot do Kubernetes pod attribution or per-seat
// mesh pricing, because both need the whole set; see the package doc.
func (e *Estimator) Annotate(ctx context.Context, in <-chan core.Asset) <-chan core.Asset {
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
				select {
				case out <- e.Estimate(a).ApplyTo(a):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out
}

// AnnotateSlice is the buffered equivalent, for the report path and for the
// server's POST handler, where the assets already exist as a slice. It returns
// a new slice; the input is not modified.
func (e *Estimator) AnnotateSlice(assets []core.Asset) []core.Asset {
	out := make([]core.Asset, len(assets))
	for i, a := range assets {
		out[i] = e.Estimate(a).ApplyTo(a)
	}
	return out
}
