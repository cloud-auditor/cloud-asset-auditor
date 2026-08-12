// Package demo serves a synthetic, fully deterministic multi-cloud inventory
// for the fictional company "Northwind". It exists so the tool can be
// evaluated, screenshotted, and documented with zero cloud credentials, and
// so the topology resolvers have a fixture that exercises every edge kind.
//
// Unlike every other provider it is NOT registered from an init(): Register
// is called explicitly by the CLI when --demo is passed. Registering it
// unconditionally would put "demo" in core.Registered() for every normal run,
// which changes the default provider set and puts fabricated assets one
// mistyped flag away from a real inventory.
package demo

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const providerName = "demo"

// DefaultStreamDuration is how long a full Collect takes end to end. The
// stream is deliberately paced rather than instant: the web UI's whole point
// is that rows land as they arrive, and a demo that finishes before the first
// paint shows none of that. It is a *total* budget, not a per-asset delay, so
// the fixture can grow without the demo getting slower.
const DefaultStreamDuration = 3 * time.Second

// Config drives the provider. The zero value is valid — New fills defaults.
type Config struct {
	// StreamDuration is the wall-clock budget for the whole stream. Zero
	// takes the default; negative means "no pacing at all" (what tests and
	// scripted runs want).
	StreamDuration time.Duration

	// IncludeRaw mirrors --include-raw: the fixture always carries payloads,
	// and they are stripped on the way out unless this is set.
	IncludeRaw bool
}

// Provider implements core.Provider over the built-in fixture.
type Provider struct {
	cfg Config
}

// Compile-time checks for the optional Configurable interfaces.
var (
	_ core.Provider                = (*Provider)(nil)
	_ core.ConcurrencyConfigurable = (*Provider)(nil)
	_ core.IncludeRawConfigurable  = (*Provider)(nil)
)

var registerOnce sync.Once

// Register installs the demo provider in the core registry. It is safe to
// call more than once — core.Register panics on a duplicate name, and the
// call site (a cobra PersistentPreRunE) runs once per command but several
// times across a test binary that executes the root command repeatedly.
func Register() {
	registerOnce.Do(func() {
		core.Register(providerName, func() (core.Provider, error) {
			return New(Config{StreamDuration: envStreamDuration()}), nil
		})
	})
}

// envStreamDuration reads AUDITOR_DEMO_DURATION ("0" or "0s" disables pacing,
// which is what a scripted export wants). An unparseable value falls back to
// the default rather than failing the run — this is a demo, not a gate.
func envStreamDuration() time.Duration {
	raw := os.Getenv("AUDITOR_DEMO_DURATION")
	if raw == "" {
		return DefaultStreamDuration
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return DefaultStreamDuration
	}
	if d <= 0 {
		return -1 // explicit "stream as fast as possible"
	}
	return d
}

// New builds a Provider. It never fails: there is nothing to authenticate.
func New(cfg Config) *Provider {
	if cfg.StreamDuration == 0 {
		cfg.StreamDuration = DefaultStreamDuration
	}
	return &Provider{cfg: cfg}
}

func (p *Provider) Name() string { return providerName }

// Validate always succeeds — that is the entire point of the demo provider.
func (p *Provider) Validate(context.Context) error { return nil }

// SetMaxConcurrency satisfies core.ConcurrencyConfigurable and does nothing.
// The fixture is a single ordered slice, so there is no fan-out to widen; the
// interface is implemented only so --max-concurrency isn't silently special
// for this one provider.
func (p *Provider) SetMaxConcurrency(int) {}

func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

// demoErrors are the non-fatal failures the demo reports partway through the
// stream. Every real audit has some — a token that can't see one API, a
// region that times out — and the UI has dedicated rendering for them, so a
// demo with a spotless run would leave that surface untested and unseen.
var demoErrors = []string{
	`cloudflare: list workers for account "northwind-labs": token lacks Account.Workers Scripts:Read (10000)`,
	`oci: list container instances in compartment "northwind-staging" (uk-london-1): request timed out after 30s`,
	`kubernetes: cluster "nw-stage-oke": list metrics.k8s.io/v1beta1.PodMetrics: the server could not find the requested resource`,
}

// Collect streams the fixture. Both channels close exactly once, and every
// send is ctx-cancellable so Ctrl+C stops the run promptly (invariant 2).
func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
	assets := make(chan core.Asset)
	errs := make(chan error)

	go func() {
		defer close(assets)
		defer close(errs)

		all := Assets()
		if !p.cfg.IncludeRaw {
			for i := range all {
				all[i].Raw = nil
			}
		}

		pause := perAssetPause(p.cfg.StreamDuration, len(all))
		errAt := errorOffsets(len(all))

		for i, a := range all {
			if msg, ok := errAt[i]; ok {
				select {
				case errs <- errors.New(msg):
				case <-ctx.Done():
					return
				}
			}
			select {
			case assets <- a:
			case <-ctx.Done():
				return
			}
			if !sleep(ctx, pause) {
				return
			}
		}
	}()

	return assets, errs
}

// perAssetPause spreads the total budget over the fixture. Sub-microsecond
// sleeps cost more in timer churn than they buy in visible pacing, so they
// collapse to zero.
func perAssetPause(total time.Duration, n int) time.Duration {
	if total <= 0 || n <= 0 {
		return 0
	}
	per := total / time.Duration(n)
	if per < time.Microsecond {
		return 0
	}
	return per
}

// errorOffsets spaces the demo errors evenly through the stream so the UI
// shows them arriving mid-run rather than all at once at either end.
func errorOffsets(n int) map[int]string {
	out := make(map[int]string, len(demoErrors))
	if n == 0 {
		return out
	}
	step := n / (len(demoErrors) + 1)
	for i, msg := range demoErrors {
		out[step*(i+1)] = msg
	}
	return out
}

// sleep waits for d, returning false if ctx was cancelled first.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
