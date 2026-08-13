package pricing

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// OCIFeedURL is Oracle's public price list. It needs no auth and no key, is
// documented under "Estimating Monthly Costs" in the OCI Billing guide, and
// returns the whole catalogue in one ~17 KB gzipped response. There is no
// region dimension anywhere in the document: OCI list pricing is globally
// uniform, which removes an axis every AWS/GCP/Azure price book has to carry.
//
// Filters: currencyCode and partNumber work; serviceCategory is silently
// ignored and limit returns HTTP 400. Do not add either.
const OCIFeedURL = "https://apexapps.oracle.com/pls/apex/cetools/api/v1/products/?currencyCode=USD"

// OCIBookID is the book whose rates the OCI feed can reprice. Rates in the
// hand-transcribed books have no SKU and are left alone.
const OCIBookID = "oci"

// payAsYouGo is the only pricing model the feed carries. Committed-use tiers
// (Annual/Monthly Flex, Universal Credits) are absent from the public document
// entirely, which is why the disclaimer has to say so rather than model them.
const payAsYouGo = "PAY_AS_YOU_GO"

// refreshTimeout bounds the whole conditional fetch. A price refresh must never
// be the reason an audit hangs.
const refreshTimeout = 30 * time.Second

// OCIFeed is the shape of the public price list document.
type OCIFeed struct {
	LastUpdated string       `json:"lastUpdated"`
	Items       []OCIProduct `json:"items"`
}

// OCIProduct is one SKU.
type OCIProduct struct {
	PartNumber                string        `json:"partNumber"`
	DisplayName               string        `json:"displayName"`
	MetricName                string        `json:"metricName"`
	ServiceCategory           string        `json:"serviceCategory"`
	CurrencyCodeLocalizations []OCICurrency `json:"currencyCodeLocalizations"`
}

// OCICurrency is one SKU's price list in one currency.
type OCICurrency struct {
	CurrencyCode string     `json:"currencyCode"`
	Prices       []OCIPrice `json:"prices"`
}

// OCIPrice is one tier. RangeMin and RangeMax are pointers because an untiered
// SKU omits both, and "absent" has to be distinguishable from zero — for
// RangeMin especially, since zero is the free tier's lower bound.
type OCIPrice struct {
	Model    string   `json:"model"`
	Value    float64  `json:"value"`
	RangeMin *float64 `json:"rangeMin"`
	RangeMax *float64 `json:"rangeMax"`
}

// ParseOCIFeed decodes a price list document.
func ParseOCIFeed(data []byte) (*OCIFeed, error) {
	var f OCIFeed
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("pricing: parse oci feed: %w", err)
	}
	if len(f.Items) == 0 {
		return nil, fmt.Errorf("pricing: oci feed has no items")
	}
	if f.LastUpdated == "" {
		return nil, fmt.Errorf("pricing: oci feed has no lastUpdated")
	}
	return &f, nil
}

// Product returns a SKU by part number.
func (f *OCIFeed) Product(partNumber string) (OCIProduct, bool) {
	for _, p := range f.Items {
		if p.PartNumber == partNumber {
			return p, true
		}
	}
	return OCIProduct{}, false
}

// MarginalPrice returns the price of the NEXT unit in the given currency — the
// tier with the highest rangeMin — along with how many tiers the SKU has.
//
// This is the tier trap, and misreading it is the most damaging mistake
// available in this package. OCI's feed encodes Always Free allowances as the
// FIRST tier, priced at 0, so reading prices[0] reports a load balancer, an
// Ampere core, or the first 10 GB of Object Storage as free forever. From the
// live feed:
//
//	B93030 Load Balancer Base      [(0, 0, 744), (0.0113, 744, 999999999)]
//	B93297 Compute - Standard - A1 [(0, 0, 3000), (0.01, 3000, 999999999999999)]
//
// Those allowances are tenancy-wide MONTHLY quantity tiers, and a per-asset
// estimator cannot know where a resource sits in one — the fifth load balancer
// and the first are identical when all you have is the asset. So every amount
// in the book is the marginal rate. That over-estimates a small tenancy, which
// is the survivable direction; the alternative prints a confident $0.00 for the
// first of everything.
//
// Selection is by highest rangeMin, and rangeMax is deliberately never
// consulted. The feed's "infinity" sentinels are inconsistent — 999999999 and
// 999999999999999 both appear, alongside genuine bounds like 744 and 3000 — so
// any comparison against a magic number is a bug waiting on Oracle to invent a
// third sentinel.
func (p OCIProduct) MarginalPrice(currency string) (OCIPrice, int, bool) {
	var tiers []OCIPrice
	for _, c := range p.CurrencyCodeLocalizations {
		if !strings.EqualFold(c.CurrencyCode, currency) {
			continue
		}
		for _, price := range c.Prices {
			if price.Model == payAsYouGo {
				tiers = append(tiers, price)
			}
		}
	}
	if len(tiers) == 0 {
		return OCIPrice{}, 0, false
	}
	best := tiers[0]
	for _, t := range tiers[1:] {
		if rangeMin(t) > rangeMin(best) {
			best = t
		}
	}
	return best, len(tiers), true
}

// rangeMin treats an absent lower bound as 0: an untiered SKU is its own
// marginal tier, starting at the first unit.
func rangeMin(p OCIPrice) float64 {
	if p.RangeMin == nil {
		return 0
	}
	return *p.RangeMin
}

// tierNote describes an allowance in the terms the feed states it, so a reader
// of `auditor cost --rates` can see both the number we charge and the quantity
// we are knowingly ignoring. Generated rather than hand-written precisely
// because a hand-written one goes stale the moment Oracle changes an allowance.
// The metric name is quoted verbatim rather than folded into the sentence:
// Oracle's are inconsistent in case and some already carry a parenthetical
// ("10,000 Requests per Month (first 50,000 free)"), so any attempt to inflect
// them reads as gibberish for a third of the catalogue.
func tierNote(p OCIProduct, marginal OCIPrice) string {
	return fmt.Sprintf("tiered SKU: the first %s units of %q bill at 0 (Always Free, tenancy-wide per month); "+
		"this is the marginal rate above that allowance",
		trimFloat(rangeMin(marginal)), p.MetricName)
}

// trimFloat renders a tier boundary without trailing zeros — the allowances are
// whole numbers (744, 3000, 18000) and "3000" reads better than "3000.000000".
func trimFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// Reprice returns a copy of b with every rate belonging to bookID and carrying
// a SKU re-priced from the feed, and that book's vintage set to the feed's own
// lastUpdated.
//
// It fails on the first SKU the feed doesn't have. That is how a renamed or
// retired part number surfaces: loudly, at generation time, rather than as a
// silently stale amount that nobody notices for a year. Rules and shapes are
// untouched — only rates[].amount, rates[].tier_note, and books[].vintage are
// ever machine-written.
func (b *Book) Reprice(bookID string, feed *OCIFeed) (*Book, error) {
	out := *b
	out.Rates = append([]Rate(nil), b.Rates...)
	out.Books = append([]Source(nil), b.Books...)

	for i := range out.Rates {
		r := &out.Rates[i]
		if r.Book != bookID || r.SKU == "" {
			continue
		}
		product, ok := feed.Product(r.SKU)
		if !ok {
			return nil, fmt.Errorf("pricing: rate %q: SKU %s is not in the feed "+
				"(Oracle renamed or retired it — find the replacement before shipping)", r.ID, r.SKU)
		}
		price, tiers, ok := product.MarginalPrice(b.CurrencyOf(r))
		if !ok {
			return nil, fmt.Errorf("pricing: rate %q: SKU %s has no %s price in %s",
				r.ID, r.SKU, payAsYouGo, b.CurrencyOf(r))
		}
		r.Amount = price.Value
		r.TierNote = ""
		if tiers > 1 {
			r.TierNote = tierNote(product, price)
		}
	}
	for i := range out.Books {
		if out.Books[i].ID == bookID {
			out.Books[i].Vintage = feed.LastUpdated
		}
	}
	out.index()
	// Revalidate rather than trusting the feed. A SKU that has become free
	// upstream now trips the amount > 0 rule, which is exactly right: it means
	// the tier structure changed under us and a human has to look.
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

// CachedFeed is one stored copy of an upstream price feed.
type CachedFeed struct {
	BookID    string
	FetchedAt time.Time
	ETag      string
	Vintage   string
	Payload   []byte // gzipped feed JSON, ~17 KB
}

// Cache is the subset of internal/store the refresher needs.
//
// It is declared here as an interface rather than taking a *store.Store so this
// package keeps no SQLite dependency and, more importantly, so the degradation
// paths are testable with a fake. "No network" and "no cache" are the normal
// operating conditions for this binary, not the error case, and untested
// fallbacks are not fallbacks.
type Cache interface {
	LoadPriceFeed(ctx context.Context, bookID string) (CachedFeed, bool, error)
	SavePriceFeed(ctx context.Context, feed CachedFeed) error
}

// Refresh re-prices base from the live OCI list feed, using cache to avoid
// re-downloading an unchanged document.
//
// It sends If-None-Match with the stored ETag (the feed sends a strong one), so
// the steady-state cost is a 304 with no body. The payload is stored gzipped
// because the DB is a user's config-dir file, not a warehouse.
//
// Every failure mode degrades and none of them fail the command:
//
//	network error / DNS / timeout / non-200  -> warn, use the cached copy
//	no cached copy either                    -> warn, return base unchanged
//	malformed payload                        -> warn, leave the cache intact
//	a SKU missing from an otherwise good feed -> warn, return base unchanged
//
// The last one is deliberate. A partially repriced book would carry the feed's
// vintage while holding some amounts from the embedded copy, and a vintage that
// overstates freshness is worse than one that is honestly old. `just prices`
// hard-fails on the same condition, which is where it gets fixed.
//
// base may be nil, in which case the embedded book is used. cache may be nil,
// in which case every fetch is unconditional and nothing is stored.
func Refresh(ctx context.Context, base *Book, cache Cache, hc *http.Client) (*Book, error) {
	if base == nil {
		d, err := Default()
		if err != nil {
			return nil, err
		}
		base = d
	}
	if hc == nil {
		hc = &http.Client{Timeout: refreshTimeout}
	}

	var cached CachedFeed
	haveCached := false
	if cache != nil {
		c, ok, err := cache.LoadPriceFeed(ctx, OCIBookID)
		switch {
		case err != nil:
			slog.Warn("price cache unreadable; fetching unconditionally", "error", err)
		case ok:
			cached, haveCached = c, true
		}
	}

	// Candidates come back in preference order (network, then cache) and the
	// first that *parses* wins, so a malformed response falls through to the
	// last good copy instead of poisoning it.
	var feed *OCIFeed
	var chosen feedCandidate
	for _, c := range feedCandidates(ctx, hc, cached, haveCached) {
		f, err := ParseOCIFeed(c.data)
		if err != nil {
			slog.Warn("price feed is malformed; trying the previous copy",
				"from_network", c.fromNetwork, "error", err)
			continue
		}
		feed, chosen = f, c
		break
	}
	if feed == nil {
		slog.Warn("price refresh produced no usable feed; using the embedded price book",
			"vintage", vintageOf(base, OCIBookID))
		return base, nil
	}

	repriced, err := base.Reprice(OCIBookID, feed)
	if err != nil {
		slog.Warn("price feed no longer covers every SKU in the book; using the embedded price book",
			"error", err, "vintage", vintageOf(base, OCIBookID))
		return base, nil
	}

	// Only a freshly fetched document is worth writing back; re-storing the
	// copy we just read would churn the DB for nothing.
	if cache != nil && chosen.fromNetwork {
		gz, err := gzipBytes(chosen.data)
		if err == nil {
			err = cache.SavePriceFeed(ctx, CachedFeed{
				BookID:    OCIBookID,
				FetchedAt: time.Now(),
				ETag:      chosen.etag,
				Vintage:   feed.LastUpdated,
				Payload:   gz,
			})
		}
		if err != nil {
			// Caching is an optimisation; the prices are already in hand.
			slog.Warn("could not cache the price feed", "error", err)
		}
	}
	slog.Debug("price book refreshed", "book", OCIBookID, "vintage", feed.LastUpdated, "skus", len(feed.Items))
	return repriced, nil
}

// feedCandidate is one possible source of feed JSON, uncompressed.
type feedCandidate struct {
	data        []byte
	fromNetwork bool
	etag        string
}

// feedCandidates performs the conditional GET and returns every usable source
// of feed JSON in preference order: the network response first when there is
// one, then the cached copy. Returning both rather than picking here is what
// lets the caller fall back on a *parse* failure, not just a transport one.
func feedCandidates(ctx context.Context, hc *http.Client, cached CachedFeed, haveCached bool) []feedCandidate {
	var out []feedCandidate
	if body, etag, ok := fetchFeed(ctx, hc, cached, haveCached); ok {
		out = append(out, feedCandidate{data: body, fromNetwork: true, etag: etag})
	}
	if haveCached {
		raw, err := gunzipBytes(cached.Payload)
		if err != nil {
			slog.Warn("cached price feed is corrupt", "error", err)
		} else {
			out = append(out, feedCandidate{data: raw, etag: cached.ETag})
		}
	}
	return out
}

// fetchFeed performs the conditional GET. A false second return means the
// network produced nothing usable — including the 304 case, where "nothing
// usable" is the correct answer because the cached copy is the response.
func fetchFeed(ctx context.Context, hc *http.Client, cached CachedFeed, haveCached bool) ([]byte, string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OCIFeedURL, nil)
	if err != nil {
		slog.Warn("price refresh: bad request", "error", err)
		return nil, "", false
	}
	if haveCached && cached.ETag != "" {
		req.Header.Set("If-None-Match", cached.ETag)
	}
	resp, err := hc.Do(req)
	if err != nil {
		slog.Warn("price refresh: fetch failed; falling back", "url", OCIFeedURL, "error", err)
		return nil, "", false
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		slog.Debug("price feed unchanged", "etag", cached.ETag)
		return nil, "", false
	case resp.StatusCode != http.StatusOK:
		slog.Warn("price refresh: unexpected status; falling back", "status", resp.Status)
		return nil, "", false
	}
	// The document is ~195 KB uncompressed; the cap guards against a redirect
	// to something enormous, not a real expectation.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		slog.Warn("price refresh: read failed; falling back", "error", err)
		return nil, "", false
	}
	return body, resp.Header.Get("ETag"), true
}

func vintageOf(b *Book, bookID string) string {
	if s, ok := b.Source(bookID); ok {
		return s.Vintage
	}
	return "unknown"
}

func gzipBytes(data []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		_ = zw.Close()
		return nil, fmt.Errorf("pricing: gzip: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("pricing: gzip: %w", err)
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("pricing: gunzip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	out, err := io.ReadAll(io.LimitReader(zr, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("pricing: gunzip: %w", err)
	}
	return out, nil
}
