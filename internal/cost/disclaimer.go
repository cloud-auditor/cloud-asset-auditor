package cost

// The caveat text lives here and nowhere else. Every surface that shows a cost
// figure — the CLI report, the JSON API, XLSX, the HTML report, the web UI —
// renders one of these two constants verbatim. A second copy would drift, and a
// drifted disclaimer is worse than none: it reads as deliberate precision.
//
// Disclaimer is the full text; DisclaimerShort is the one-line version for
// places where a paragraph would corrupt the output (a CSV cell, a table row, a
// log line). Neither is optional anywhere a number appears.

// Disclaimer is the canonical statement of what a cost figure from this tool
// is not. Rendered above the first table of `auditor cost`, as a required
// top-level field of the JSON report, and merged across row 1 of an XLSX
// summary sheet — always before any figure, never as a footer.
const Disclaimer = `This is a list-price estimate. It is not an invoice, and it will not match your bill.

It does not account for:
  - Your negotiated rate. Figures marked ~ use public list price. Universal
    Credits, Enterprise Agreements, private pricing and partner discounts are
    invisible to this tool. Only measured rows carry your real, post-discount cost.
  - Committed-use discounts. OCI Annual/Monthly Flex, reserved capacity. The OCI
    list feed exposes only pay-as-you-go; there is no commitment tier in the data.
  - Free-tier allowances. These are tenancy-wide monthly tiers, and a per-asset
    estimator cannot know where a given resource falls in one. Every rate here is
    the MARGINAL rate — the price of the next unit. Your first load balancer, your
    first 3,000 A1 OCPU-hours and your first 10 GB of Object Storage may in fact be
    free. This tool over-estimates small tenancies for exactly that reason.
  - Egress and data transfer. Not modelled anywhere, for any provider.
  - Request- and consumption-based charges. R2 operations, KV reads and writes, D1
    rows, Workers requests and CPU-ms, Object Storage requests, load balancer
    bandwidth. These are shown as "metered", never as 0.
  - Support plans, taxes, and currency conversion. EUR (NetBird) and USD
    (everything else) are reported separately and never combined; no exchange rate
    is applied anywhere in this tool.
  - Plan tiers this tool cannot observe. A Cloudflare zone's plan, and your
    Tailscale or NetBird plan, are not exposed by the APIs it calls. Where the plan
    changes the answer, every tier is shown.
  - Time. Every figure is a rate for a hypothetical full month at the current
    configuration. It is not a forecast, and it is not what you spent.

measured rows are different: they are your actual billed amount for a completed
past month, read from the provider's billing API including your discount. They
are historical fact, not prediction — next month may differ.

Use this to find the shape of your spend and the things nobody is watching. Do
not use it to reconcile an invoice, set a budget, or bill a customer.`

// DisclaimerShort is the single line that travels with an individual total —
// in a CSV row, beneath a table's TOTAL, in a Slack paste. It names the four
// exclusions most likely to make a number wrong by a large factor.
const DisclaimerShort = "Estimate, not an invoice: list price, excluding negotiated and committed-use " +
	"discounts, free-tier allowances, egress, and request-based charges."
