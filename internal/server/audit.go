package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

// runProviders is the server-side equivalent of internal/cli/audit.go's
// runProviders + selectProviders + applyProviderOptions, collapsed into one
// helper. Returns fanned-in assets + errors channels (both closed when work
// is done). initErrors holds factory-time failures (e.g. missing tokens) —
// we can't push these through the error channel because they happen before
// it exists, and they're useful to render distinctly in the UI.
func (s *Server) runProviders(ctx context.Context, names []string) (assets <-chan core.Asset, errs <-chan error, initErrors []string) {
	selected, initErrors := s.selectProviders(names)

	a := make(chan core.Asset)
	e := make(chan error)

	if len(selected) == 0 {
		close(a)
		close(e)
		return a, e, initErrors
	}

	var wg sync.WaitGroup
	for _, p := range selected {
		p := p
		wg.Add(1)
		go func() {
			defer wg.Done()
			pAssets, pErrs := p.Collect(ctx)
			forward(ctx, pAssets, pErrs, a, e)
		}()
	}
	go func() {
		wg.Wait()
		close(a)
		close(e)
	}()
	return a, e, initErrors
}

// selectProviders mirrors the CLI's selection logic but accumulates
// factory failures in a slice (returned to the caller) instead of writing
// to stderr — handlers route them into SSE events or HTTP responses.
func (s *Server) selectProviders(names []string) ([]core.Provider, []string) {
	if len(names) == 0 {
		names = core.Registered()
	}

	var (
		out      = make([]core.Provider, 0, len(names))
		initErrs []string
	)
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || strings.EqualFold(n, "none") {
			continue
		}
		factory, ok := core.Lookup(n)
		if !ok {
			initErrs = append(initErrs, fmt.Sprintf("provider %q is not registered", n))
			continue
		}
		p, err := factory()
		if err != nil {
			initErrs = append(initErrs, fmt.Sprintf("provider %q failed to initialize: %v", n, err))
			continue
		}
		s.applyProviderOptions(p)
		out = append(out, p)
	}
	return out, initErrs
}

// applyProviderOptions pushes the server's configured per-provider knobs.
// For the web UI, regions / kube context / etc. come from the operator's
// env at server startup, not from the browser — those credentials must
// not be controllable from the client side.
func (s *Server) applyProviderOptions(p core.Provider) {
	if c, ok := p.(core.ConcurrencyConfigurable); ok && s.cfg.MaxConcurrency > 0 {
		c.SetMaxConcurrency(s.cfg.MaxConcurrency)
	}
	if c, ok := p.(core.IncludeRawConfigurable); ok {
		c.SetIncludeRaw(s.cfg.IncludeRaw)
	}
	if c, ok := p.(core.ProfileConfigurable); ok {
		c.SetProfile(os.Getenv("OCI_CLI_PROFILE"))
	}
	// Regions / kube context are server-startup config, not per-request.
}

// forward copies values from one provider's channels onto the fan-in
// channels until both source channels close or ctx is cancelled.
func forward(
	ctx context.Context,
	srcAssets <-chan core.Asset, srcErrs <-chan error,
	dstAssets chan<- core.Asset, dstErrs chan<- error,
) {
	for srcAssets != nil || srcErrs != nil {
		select {
		case <-ctx.Done():
			return
		case a, ok := <-srcAssets:
			if !ok {
				srcAssets = nil
				continue
			}
			select {
			case dstAssets <- a:
			case <-ctx.Done():
				return
			}
		case er, ok := <-srcErrs:
			if !ok {
				srcErrs = nil
				continue
			}
			if er == nil {
				continue
			}
			select {
			case dstErrs <- er:
			case <-ctx.Done():
				return
			}
		}
	}
}
