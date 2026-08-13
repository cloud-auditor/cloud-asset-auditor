package cli

import (
	"bufio"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/cloud-auditor/cloud-asset-auditor/internal/cost"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/filter"
	"github.com/cloud-auditor/cloud-asset-auditor/internal/pricing"
)

// staleBookAge is when a price book starts warning about its own age. Ninety
// days is roughly one OCI list-price revision cycle: long enough that a fresh
// release doesn't nag, short enough that a binary somebody has been carrying
// for a year says so before its numbers get read out in a meeting.
const staleBookAge = 90 * 24 * time.Hour

// costFormats mirrors internal/cost's renderer switch so a typo'd -o is caught
// before a ten-minute audit runs rather than after it — the same reason
// `check` resolves its renderer up front.
var costFormats = []string{"table", "json", "csv", "markdown"}

func newCostCmd(s *cliState) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Estimate monthly cost from the inventory.",
		Long: `Prices the inventory against a public list-price book and reports the total,
the biggest line items, and — just as importantly — everything it could not
price.

Figures are LIST-PRICE ESTIMATES, not an invoice. One character carries that:
a leading ~ means this tool computed the number from a price book, and a number
without one came from the provider's own billing API. Resources whose billing
is consumption-based read "metered"; resources the book has no rule for read
"unknown". Neither is ever rendered as 0, because a 0 would be a claim that
something is free and this tool does not make that claim. The full disclaimer
prints above every report.

Like ` + "`topology`" + ` and ` + "`reach`" + `, this forces --include-raw on: Kubernetes pod
attribution reads resources.requests out of Raw, and volume sizes come from
there when the provider didn't tag them. A report built without Raw would
quietly under-count rather than fail, which is the worse failure.

There is deliberately no --exit-code and no budget threshold. Failing a
pipeline on an estimate teaches people the estimate is authoritative; gate on
` + "`reach --exposed`" + ` and ` + "`diff`" + `, which are statements of fact.

Examples:
  auditor cost                                      # live audit, table report
  auditor cost --provider oci --group-by region
  auditor cost --from-snapshot assets.json -o json
  auditor cost --rates                              # print the loaded price book and exit
  auditor cost --top 0 --show-unpriced              # every asset, priced or not
  auditor cost --price-book my-rates.yaml           # merge an override over the built-in book
  auditor cost --refresh-prices                     # fetch current OCI list prices first
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := s.v.BindPFlags(cmd.Flags()); err != nil {
				return fmt.Errorf("bind flags: %w", err)
			}
			v := s.v

			format := strings.ToLower(v.GetString("output"))
			if format == "md" {
				format = "markdown"
			}
			if !slices.Contains(costFormats, format) {
				return fmt.Errorf("unknown cost output format %q (supported: %s)",
					format, strings.Join(costFormats, ", "))
			}

			// Validated up front, like the format, so a typo costs a second
			// rather than a ten-minute audit.
			groupBy, err := cost.ParseGroupBy(v.GetString("group-by"))
			if err != nil {
				return err
			}

			assetFilter, err := filter.Parse(v.GetStringSlice("filter"))
			if err != nil {
				return err
			}

			w, closeOut, err := openOutput(v.GetString("output-file"))
			if err != nil {
				return err
			}
			defer closeOut()

			book, err := loadPriceBook(cmd.Context(), v)
			if err != nil {
				return err
			}

			// --rates is a question about the book, not about the estate, so it
			// answers before a provider is touched. That makes it the one way to
			// audit what this tool believes things cost without credentials.
			if v.GetBool("rates") {
				return renderRates(w, book)
			}

			estimator := cost.New(book)

			collected, provErrs, err := s.gatherForGraph(cmd.Context(), v)
			if err != nil {
				return err
			}

			report := estimator.Report(assetFilter.Slice(collected), cost.Options{
				GroupBy:      groupBy,
				TopN:         v.GetInt("top"),
				ShowUnpriced: v.GetBool("show-unpriced"),
			})
			if err := cost.Render(report, format, w); err != nil {
				return err
			}

			// Provider failures surface after the report, not instead of it. A
			// partial inventory still prices, and the report's own coverage line
			// is what tells the reader how partial it was.
			if len(provErrs) > 0 {
				return errors.Join(append([]error{ErrPartial}, provErrs...)...)
			}
			return nil
		},
	}

	addPriceBookFlags(cmd)
	cmd.Flags().Bool("rates", false,
		"print the loaded price book (rate card, tier notes, book vintages) and exit — no audit, no credentials")
	cmd.Flags().Bool("refresh-prices", false,
		"fetch current OCI list prices before pricing (a network call to Oracle's public price API; never implicit)")

	cmd.Flags().StringP("output", "o", "table", "report format: table|json|csv|markdown")
	cmd.Flags().String("output-file", "", "write the report to this file instead of stdout")
	cmd.Flags().String("group-by", "provider",
		"roll totals up by: provider|type|region|account|tag:KEY")
	cmd.Flags().Int("top", 20, "list the N most expensive assets (0 = all)")
	cmd.Flags().Bool("show-unpriced", false,
		"list every metered/unknown asset individually instead of counting them by type")
	cmd.Flags().StringArray("filter", nil,
		`price matching assets only: key=value[,value...] / key!=... with key provider|account|region|type|id|name|status|tag:KEY and glob values; repeatable (ANDed)`)

	addGraphSourceFlags(cmd)
	return cmd
}

// addPriceBookFlags registers the flags that decide *which prices are used*.
// Shared by `cost` and `audit --cost` so an override book reaches both: a price
// book that applied to the report but not to the spreadsheet built from the
// same estate would put two different numbers in front of one reader.
func addPriceBookFlags(cmd *cobra.Command) {
	cmd.Flags().StringSlice("price-book", nil,
		"price-book YAML to merge over the built-in book (repeatable; later files win by rate id / rule type)")
	// Default 0 rather than 730 so the flag means "override" instead of
	// "always set": a --price-book declaring hours_per_month: 744 must not be
	// silently overwritten by a flag the user never typed. Zero can't be a
	// legitimate value anyway — it would multiply every hourly rate to nothing.
	cmd.Flags().Float64("hours-per-month", 0,
		"override the price book's hours in a billing month for hourly rates (book default: 730 = 365*24/12)")
}

// loadPriceBook resolves the book for this run: the embedded default with each
// --price-book file merged over it, optionally re-priced from the live OCI
// feed, with --hours-per-month applied last.
//
// Offline is the assumed operating condition, not the error case. With no
// network and no --refresh-prices this returns exactly the prices that shipped
// with the binary, and warns when they are old enough to matter.
func loadPriceBook(ctx context.Context, v *viper.Viper) (*pricing.Book, error) {
	// LoadFile builds a fresh book rather than handing back the shared
	// Default(), so the hours override below is safe to write onto it.
	book, err := pricing.LoadFile(v.GetStringSlice("price-book")...)
	if err != nil {
		return nil, err
	}

	if v.GetBool("refresh-prices") {
		// The cache is nil until the store grows its price_feeds table, so
		// every --refresh-prices is an unconditional fetch for now. Refresh
		// degrades on all of its failure paths rather than erroring, so a
		// blocked egress policy costs a warning and the embedded prices — not
		// the command.
		refreshed, err := pricing.Refresh(ctx, book, nil, nil)
		if err != nil {
			return nil, err
		}
		book = refreshed
	}

	switch hours := v.GetFloat64("hours-per-month"); {
	case hours > 0:
		book.HoursPerMonth = hours
	case hours < 0:
		return nil, fmt.Errorf("--hours-per-month must be > 0, got %v", hours)
	}

	// `auditor cost` also reports stale books in the report body, but
	// `audit --cost` has no report — the tags go straight into a spreadsheet
	// or an API response — so stderr is the only place their age can be said.
	for _, src := range book.Stale(time.Now(), staleBookAge) {
		age := "unreadable"
		if t, ok := src.VintageTime(); ok {
			age = fmt.Sprintf("%d days", int(time.Since(t).Hours()/24))
		}
		slog.Warn("price book is old; its prices may have moved since",
			"book", src.ID, "vintage", src.Vintage, "age", age,
			"hint", "run `auditor cost --refresh-prices`, or upgrade the binary")
	}
	return book, nil
}

// renderRates prints the loaded rate card. It lives here rather than in
// internal/cost because it describes the *book*, not a report — nothing about
// it depends on having audited anything, which is the whole point of --rates:
// it is the one way to see what this tool believes things cost without handing
// it a single credential.
//
// Every rate is printed with its tier note, because the note is what makes the
// number defensible. OCI encodes Always Free allowances as a cheaper first
// tier and the book always quotes the LAST tier, so "$0.0113/hr" alone would
// read as the price of a user's first load balancer when it is the price of
// their second.
func renderRates(w io.Writer, b *pricing.Book) error {
	bw := bufio.NewWriter(w)

	fmt.Fprintf(bw, "PRICE BOOK  (schema v%d, %s, %g hours/month)\n\n",
		b.Version, b.Currency, b.HoursPerMonth)
	fmt.Fprintln(bw, cost.DisclaimerShort)
	fmt.Fprintln(bw)

	fmt.Fprintln(bw, "BOOKS")
	for _, src := range b.Books {
		fmt.Fprintf(bw, "  %-12s %s\n", src.ID, src.Vintage)
		fmt.Fprintf(bw, "  %-12s %s\n", "", src.Source)
		if src.GeneratedBy != "" {
			fmt.Fprintf(bw, "  %-12s regenerate with: %s\n", "", src.GeneratedBy)
		}
		if src.Note != "" {
			fmt.Fprintf(bw, "  %-12s %s\n", "", src.Note)
		}
	}

	// Sorted by (book, id) rather than file order so two runs — and two
	// different --price-book merges — produce comparable output.
	rates := slices.SortedFunc(slices.Values(b.Rates), func(x, y pricing.Rate) int {
		if c := cmp.Compare(x.Book, y.Book); c != 0 {
			return c
		}
		return cmp.Compare(x.ID, y.ID)
	})

	idWidth := 0
	for _, r := range rates {
		idWidth = max(idWidth, len(r.ID))
	}

	fmt.Fprintf(bw, "\nRATES (%d)  — every amount is the MARGINAL rate: the price of the next unit, not the first\n", len(rates))
	for _, r := range rates {
		sku := r.SKU
		if sku == "" {
			sku = "—"
		}
		fmt.Fprintf(bw, "  %-*s  %-10s %10.6f %s/%s\n",
			idWidth, r.ID, sku, r.Amount, b.CurrencyOf(&r), r.Unit)
		for _, note := range []string{r.TierNote, r.Note} {
			if note != "" {
				fmt.Fprintf(bw, "  %-*s  %s\n", idWidth, "", note)
			}
		}
	}

	fmt.Fprintf(bw, "\n%d rules over %d shape table%s; run `auditor cost` to see what they price.\n",
		len(b.Rules), len(b.Shapes), pluralS(len(b.Shapes)))
	return bw.Flush()
}

// newAuditEstimator builds the annotator for `audit --cost`, or returns nil
// when the flag is off. That nil is the off-by-default switch: every caller
// treats it as "leave the stream alone", so a plain audit emits exactly the
// bytes it did before this feature existed.
//
// It also emits the one-time limitation notice. Warn rather than Info because
// this is disclosure, not progress — it has to survive --log-level warn, which
// anyone running the binary in CI is likely to have set.
func newAuditEstimator(ctx context.Context, v *viper.Viper) (*cost.Estimator, error) {
	if !v.GetBool("cost") {
		return nil, nil
	}
	book, err := loadPriceBook(ctx, v)
	if err != nil {
		return nil, err
	}
	slog.Warn("--cost stamps per-asset estimates only: Kubernetes pod attribution "+
		"and per-seat mesh rollups need the whole set, so they are absent here",
		"hint", "run `auditor cost` for those")
	return cost.New(book), nil
}
