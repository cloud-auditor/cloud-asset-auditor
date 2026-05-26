# Adding a new provider

Cloud-asset-auditor's provider contract is intentionally small. A new
provider typically lives in `internal/providers/<name>/` and is well under
200 lines of orchestration glue plus per-resource mapping functions.

This guide walks through scaffolding a hypothetical `example` provider
end-to-end.

## The contract

The base interface in [`internal/core/provider.go`](../internal/core/provider.go):

```go
type Provider interface {
    Name() string
    Validate(ctx context.Context) error
    Collect(ctx context.Context) (<-chan Asset, <-chan error)
}
```

Two **invariants** baked into the channel-based `Collect`:

1. **Both channels close.** The implementation must close both `assets`
   and `errs` when work finishes (or `ctx` is cancelled). The CLI relies
   on those closures to terminate the render loop.
2. **Errors are non-fatal.** Push each recoverable failure onto `errs`
   and keep going — `internal/cli/audit.go::runProviders` fans errors in
   alongside assets and the CLI maps "some errors, some assets" to
   exit code 2 ("partial provider failure"). Don't `panic` and don't
   bail out of `Collect` on the first error.

Optional **Configurable** interfaces let the CLI push flag values down
without growing the base contract:

| Interface                | Method                              | Wired to                                                 |
| ------------------------ | ----------------------------------- | -------------------------------------------------------- |
| `ConcurrencyConfigurable`| `SetMaxConcurrency(int)`            | `--max-concurrency`                                      |
| `IncludeRawConfigurable` | `SetIncludeRaw(bool)`               | `--include-raw`                                          |
| `ProfileConfigurable`    | `SetProfile(string)`                | `--oci-profile`                                          |
| `RegionsConfigurable`    | `SetRegions([]string)`              | `--oci-regions`                                          |
| `KubeConfigurable`       | `SetKubeContext/Namespace/...`      | `--kube-context` + `--kube-namespace` + `--kube-exclude-namespaces` |

Implement only the ones your provider cares about. The CLI type-asserts
each one in `applyProviderOptions` and silently skips when the assertion
fails.

## Step 1 — Package skeleton

Create `internal/providers/example/example.go`:

```go
package example

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "os"

    "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

const (
    providerName          = "example"
    defaultMaxConcurrency = 5
)

type Config struct {
    APIToken       string
    MaxConcurrency int
    IncludeRaw     bool
}

type Provider struct {
    client *exampleSDKClient // your real SDK type
    cfg    Config
}

// Compile-time interface checks. If these break, the build catches it.
var (
    _ core.Provider                = (*Provider)(nil)
    _ core.ConcurrencyConfigurable = (*Provider)(nil)
    _ core.IncludeRawConfigurable  = (*Provider)(nil)
)

// init registers the provider so blank-importing the package wires it in.
// Read env at factory time — missing creds surface as a warning via the
// CLI's selectProviders path, not a startup panic.
func init() {
    core.Register(providerName, func() (core.Provider, error) {
        return New(Config{APIToken: os.Getenv("EXAMPLE_API_TOKEN")})
    })
}

func New(cfg Config) (*Provider, error) {
    if cfg.APIToken == "" {
        return nil, errors.New("example: EXAMPLE_API_TOKEN is not set")
    }
    if cfg.MaxConcurrency <= 0 {
        cfg.MaxConcurrency = defaultMaxConcurrency
    }
    return &Provider{
        client: exampleSDK.NewClient(cfg.APIToken),
        cfg:    cfg,
    }, nil
}

func (p *Provider) Name() string { return providerName }

func (p *Provider) SetMaxConcurrency(n int) {
    if n > 0 { p.cfg.MaxConcurrency = n }
}
func (p *Provider) SetIncludeRaw(b bool) { p.cfg.IncludeRaw = b }

func (p *Provider) Validate(ctx context.Context) error {
    // Pick the cheapest unambiguous call the SDK offers — a /me, /whoami,
    // /version, or list-of-one. Avoid full enumeration.
    if _, err := p.client.Whoami(ctx); err != nil {
        return fmt.Errorf("example: validate token: %w", err)
    }
    return nil
}

// rawOf marshals v for Asset.Raw when --include-raw is set.
func (p *Provider) rawOf(v any) json.RawMessage {
    if !p.cfg.IncludeRaw { return nil }
    b, err := json.Marshal(v)
    if err != nil { return nil }
    return b
}
```

## Step 2 — The Collect orchestrator

`internal/providers/example/collect.go`:

```go
package example

import (
    "context"
    "errors"
    "fmt"

    "golang.org/x/sync/errgroup"

    "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func (p *Provider) Collect(ctx context.Context) (<-chan core.Asset, <-chan error) {
    assets := make(chan core.Asset)
    errs := make(chan error, 32)
    go func() {
        defer close(assets)
        defer close(errs)
        p.run(ctx, assets, errs)
    }()
    return assets, errs
}

func (p *Provider) run(ctx context.Context, assets chan<- core.Asset, errs chan<- error) {
    g, gctx := errgroup.WithContext(ctx)
    if p.cfg.MaxConcurrency > 0 {
        g.SetLimit(p.cfg.MaxConcurrency)
    }

    // collect wraps each per-resource function so its error flows into
    // `errs` instead of returning from g.Go (which would cancel siblings).
    collect := func(name string, fn func(context.Context) error) {
        g.Go(func() error {
            if err := fn(gctx); err != nil && !errors.Is(err, context.Canceled) {
                select {
                case errs <- fmt.Errorf("example %s: %w", name, err):
                case <-gctx.Done():
                }
            }
            return nil
        })
    }

    collect("widgets",   func(c context.Context) error { return p.collectWidgets(c, assets) })
    collect("gadgets",   func(c context.Context) error { return p.collectGadgets(c, assets) })

    _ = g.Wait()
}
```

Why `errgroup` instead of plain goroutines + WaitGroup? Two reasons:

- `SetLimit` provides the per-provider concurrency cap that the
  `--max-concurrency` flag controls.
- `WithContext` cancels siblings on first non-nil return — we exploit
  this for "real" failures (auth errors, ctx cancellation) while
  routing per-resource failures through `errs` so they don't trigger
  the cancellation.

## Step 3 — Per-resource collectors

One file per resource type keeps diffs scoped. `internal/providers/example/widgets.go`:

```go
package example

import (
    "context"
    "fmt"

    "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func (p *Provider) collectWidgets(ctx context.Context, out chan<- core.Asset) error {
    iter := p.client.Widgets.List(ctx)
    for iter.Next() {
        if !sendAsset(ctx, out, p.widgetToAsset(iter.Current())) {
            return nil
        }
    }
    if err := iter.Err(); err != nil {
        return fmt.Errorf("list widgets: %w", err)
    }
    return nil
}

func (p *Provider) widgetToAsset(w sdk.Widget) core.Asset {
    return core.Asset{
        Provider:  providerName,
        AccountID: w.AccountID,
        Type:      "example.widget",
        ID:        w.ID,
        Name:      w.Name,
        Status:    w.Status,
        CreatedAt: &w.CreatedAt,
        Tags: map[string]string{
            "color": w.Color,
            "size":  fmt.Sprintf("%d", w.Size),
        },
        Raw: p.rawOf(w),
    }
}

// sendAsset is the ctx-cancel-aware channel send every collector uses.
// Lifted from the project's shared helpers — see internal/providers/oci/oci.go
// for the canonical one-liner.
func sendAsset(ctx context.Context, out chan<- core.Asset, a core.Asset) bool {
    select {
    case <-ctx.Done(): return false
    case out <- a:     return true
    }
}
```

### Asset.Type convention

`<provider>.<resource>` — lowercase, dot-separated. Examples:

| Provider     | Resource             | Asset.Type                       |
| ------------ | -------------------- | -------------------------------- |
| `cloudflare` | DNS record           | `cloudflare.dns_record`          |
| `oci`        | Compute instance     | `oci.compute.instance`           |
| `kubernetes` | apps/v1 Deployment   | `apps/v1.Deployment`             |
| `example`    | Widget               | `example.widget`                 |

Kubernetes is the exception — its Asset.Type encodes the GVR
(`<group>/<version>.<kind>`) because that's what makes CRDs work.

## Step 4 — Wire into the binary

Blank-import the new package in `cmd/auditor/main.go`:

```go
import (
    _ "github.com/cloud-auditor/cloud-asset-auditor/internal/providers/example"
)
```

That single line is the only place outside the new package you need to
touch — `init()` does the registration; the CLI auto-discovers everything
in `core.Registered()`.

## Step 5 — Tests

Two layers, matching what every existing provider does:

```go
// internal/providers/example/example_test.go
package example

import (
    "encoding/json"
    "testing"
    "time"

    "github.com/cloud-auditor/cloud-asset-auditor/internal/core"
)

func TestNew_RequiresToken(t *testing.T) {
    if _, err := New(Config{}); err == nil {
        t.Fatal("expected error when APIToken is empty")
    }
}

func TestWidgetToAsset(t *testing.T) {
    p := &Provider{}
    created := time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)
    a := p.widgetToAsset(sdk.Widget{
        ID: "w-1", Name: "blue-1", AccountID: "acct",
        Status: "active", CreatedAt: created,
        Color: "blue", Size: 7,
    })
    if a.Type != "example.widget" { t.Errorf("Type = %q", a.Type) }
    if a.Tags["color"] != "blue"  { t.Errorf("color tag = %q", a.Tags["color"]) }
    // ...
}

func TestRawOf_RoundTrip(t *testing.T) {
    p := &Provider{cfg: Config{IncludeRaw: true}}
    raw := p.rawOf(map[string]string{"a": "1"})
    var back map[string]string
    if err := json.Unmarshal(raw, &back); err != nil || back["a"] != "1" {
        t.Fatalf("Raw round-trip failed: %v / %s", err, raw)
    }
}

func TestInit_RegistersProvider(t *testing.T) {
    if _, ok := core.Lookup("example"); !ok {
        t.Fatal("provider not registered by init()")
    }
}
```

**What to test, what to skip:**

- Test the mapping functions (pure, easy, high signal).
- Test factory defaults + the optional Configurable interface setters.
- Test that `init()` registered the provider name.
- **Skip** mocking the SDK for collector tests in the baseline ship —
  put that in an integration test behind a `//go:build integration`
  tag (see init-plan.md §5).

## Step 6 — Docs

- Add a row to the resource matrix in [`providers.md`](./providers.md).
- Document the minimum permissions / scopes.
- If your provider adds new env vars or flags, update
  [`configuration.md`](./configuration.md).

## Step 7 — That's it

`just build && just test` and you've got a new provider. The CLI,
web UI, CSV/JSON renderers, exit-code semantics, partial-failure
handling, Helm chart, and Docker image all pick it up for free.

## Reference reading order

The existing providers, ranked from "simplest to follow" to "most
complex":

1. [`cloudflare`](../internal/providers/cloudflare/) — single account, paginated SDK, per-zone fan-out.
2. [`oci`](../internal/providers/oci/) — auth chain, compartment recursion, per-(region × compartment) fan-out.
3. [`kubernetes`](../internal/providers/kubernetes/) — dynamic-client + discovery (the "no per-resource code" pattern).
