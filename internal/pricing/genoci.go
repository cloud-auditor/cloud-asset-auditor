//go:build ignore

// Command genoci regenerates internal/pricing/books/oci.yaml from Oracle's
// public price list, and records the feed it used in testdata/.
//
// Run it through `just prices`; it is excluded from the normal build by the
// ignore tag, the same way a generator should be.
//
// It rewrites only rates[].amount, rates[].tier_note and books[].vintage.
// Rate ids, SKUs, units, shapes, rules and notes are hand-curated — the point
// of a price book is that a reviewer can read it and check it against Oracle's
// page, and 648 machine-written rules would destroy that. Most of the feed's
// SKUs are for services this tool does not inventory anyway.
//
// It fails on the first SKU the feed no longer carries. That loud failure is
// the feature: a renamed part number should stop a release, not decay into a
// stale number nobody notices.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
	"gopkg.in/yaml.v3"
)

// header is re-emitted above the generated document. It lives here rather than
// in the YAML because marshalling drops comments, so every regeneration would
// otherwise strip it.
const header = `# GENERATED FILE — rates[].amount, rates[].tier_note and books[].vintage are
# rewritten by ` + "`just prices`" + ` from Oracle's public price list. Everything else
# (rate ids, SKUs, units, shapes, rules, notes) is hand-curated: edit it here
# and re-run ` + "`just prices`" + ` to re-price.
#
# Every amount is the MARGINAL tier — the price of the next unit, not the
# first. Oracle encodes Always Free allowances as the first tier at $0.00, and a
# per-asset estimator cannot know where a resource sits in a tenancy-wide
# monthly tier. See internal/pricing/refresh.go, OCIProduct.MarginalPrice.
`

func main() {
	dir := flag.String("dir", "internal/pricing", "path to the pricing package")
	feedFile := flag.String("feed", "", "read the feed from this file instead of fetching it")
	flag.Parse()

	if err := run(*dir, *feedFile); err != nil {
		log.Fatalf("genoci: %v", err)
	}
}

func run(dir, feedFile string) error {
	bookPath := filepath.Join(dir, "books", "oci.yaml")
	raw, err := os.ReadFile(bookPath)
	if err != nil {
		return err
	}
	// Load the OCI book alone, not the merged default: this file has to stay
	// self-contained, and marshalling a merged book back would fold the other
	// three books into it.
	book, err := pricing.Load(pricing.Document{Name: bookPath, Data: raw})
	if err != nil {
		return err
	}

	payload, err := feedBytes(feedFile)
	if err != nil {
		return err
	}
	feed, err := pricing.ParseOCIFeed(payload)
	if err != nil {
		return err
	}
	repriced, err := book.Reprice(pricing.OCIBookID, feed)
	if err != nil {
		return err
	}

	out, err := yaml.Marshal(repriced)
	if err != nil {
		return err
	}
	if err := os.WriteFile(bookPath, append([]byte(header), out...), 0o644); err != nil {
		return err
	}

	// Record the exact feed the book was generated from, so the marginal-tier
	// test can re-derive every amount rather than trusting the generator that
	// wrote them. Gzipped because 195 KB of JSON in git is 195 KB forever.
	if err := writeFeedFixture(filepath.Join(dir, "testdata", "oci-feed.json.gz"), payload); err != nil {
		return err
	}

	fmt.Printf("books/oci.yaml re-priced from %d SKUs, vintage %s\n", len(feed.Items), feed.LastUpdated)
	return nil
}

func feedBytes(path string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pricing.OCIFeedURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch %s: %s", pricing.OCIFeedURL, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func writeFeedFixture(path string, payload []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	// Fixed compression level and no header metadata, so re-running the
	// generator on an unchanged feed produces an unchanged file and
	// `just prices-verify` stays meaningful.
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return err
	}
	if _, err := zw.Write(payload); err != nil {
		_ = zw.Close()
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}
