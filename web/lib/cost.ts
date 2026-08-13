import type { Asset } from './types';

/**
 * Reads the `cost.*` tags the Go estimator stamps onto assets
 * (internal/cost/annotate.go). Cost rides in Tags rather than in a top-level
 * Asset field, so it reaches every renderer and this UI for free — see the
 * cost package docs for why that was the right carrier.
 *
 * The one rule this module exists to uphold, restated because it is easy to
 * undo from the UI side: **an unpriced resource must never render as 0.**
 * "$0.00" is a real price in Oracle's feed — the first tier of an Always Free
 * allowance — so zero and unknown must be impossible to confuse. Every
 * function here returns a discriminated result rather than a number, so a
 * caller cannot accidentally sum an unknown into a total.
 */

export type CostBasis = 'measured' | 'inferred' | 'assumed' | 'unpriceable' | 'unknown';

export interface AssetCost {
  /** The raw tag value, e.g. "~18.25", "412.90", "unknown", "metered". */
  raw: string;
  /** Set only when `raw` is a figure. Never set for unknown or metered. */
  amount?: number;
  currency?: string;
  basis: CostBasis;
  /** The estimator's human explanation: SKUs, rates and quantities used. */
  detail?: string;
  /** True when the figure is an estimate rather than a billed amount. */
  estimated: boolean;
}

const BASES: readonly CostBasis[] = [
  'measured',
  'inferred',
  'assumed',
  'unpriceable',
  'unknown',
];

function asBasis(v: string | undefined): CostBasis {
  return BASES.includes(v as CostBasis) ? (v as CostBasis) : 'unknown';
}

/** Returns null when the asset carries no cost annotation at all — which is
 *  the normal case, since cost is off unless the server was started with it. */
export function assetCost(a: Asset): AssetCost | null {
  const t = a.tags;
  if (!t) return null;
  const raw = t['cost.monthly'];
  if (raw === undefined) return null;

  const basis = asBasis(t['cost.basis']);
  const estimated = raw.startsWith('~');
  // Only a leading `~` and digits are a figure. "unknown" and "metered" are
  // sentinel words, and "<0.0001" is the estimator's way of saying "a positive
  // amount too small to print" — none of them may become a number here.
  const numeric = /^~?\d/.test(raw) ? Number(raw.replace(/^~/, '')) : NaN;

  return {
    raw,
    amount: Number.isFinite(numeric) ? numeric : undefined,
    currency: t['cost.currency'],
    basis,
    detail: t['cost.detail'],
    estimated,
  };
}

/** True when any asset in the set carries cost tags. Drives whether the UI
 *  shows its cost affordances at all — an empty Cost column on every row is
 *  worse than no column. */
export function hasCost(assets: readonly Asset[]): boolean {
  // Cost is all-or-nothing per run, so the first annotated asset settles it.
  // Scanning the whole set on every render of a 50k-row table would not.
  for (const a of assets) {
    if (a.tags?.['cost.monthly'] !== undefined) return true;
  }
  return false;
}

export interface CostTotals {
  /** Summed amounts, per currency. Currencies are never combined — no
   *  exchange rate is applied anywhere in this tool. */
  byCurrency: Map<string, number>;
  priced: number;
  /** Consumption-based: real charges this tool structurally cannot see. */
  metered: number;
  /** No rule matched. A gap in the price book, NOT a free resource. */
  unknown: number;
  /** Assets carrying no cost annotation at all. */
  unannotated: number;
}

/**
 * Rolls a set up per currency.
 *
 * `metered` and `unknown` are counted, never coerced to zero and summed. A
 * total that silently swallowed them would understate spend by exactly the
 * amount nobody is watching — which is the spend this feature exists to find.
 */
export function costTotals(assets: readonly Asset[]): CostTotals {
  const out: CostTotals = {
    byCurrency: new Map(),
    priced: 0,
    metered: 0,
    unknown: 0,
    unannotated: 0,
  };
  for (const a of assets) {
    const c = assetCost(a);
    if (!c) {
      out.unannotated += 1;
      continue;
    }
    if (c.amount !== undefined) {
      const cur = c.currency ?? 'USD';
      out.byCurrency.set(cur, (out.byCurrency.get(cur) ?? 0) + c.amount);
      out.priced += 1;
    } else if (c.basis === 'unpriceable') {
      out.metered += 1;
    } else {
      out.unknown += 1;
    }
  }
  return out;
}

const MONEY = new Map<string, Intl.NumberFormat>();

/** Currency formatting with the locale pinned — see web/lib/format.ts for why
 *  a bare toLocaleString broke the build's reproducibility. */
export function fmtMoney(amount: number, currency = 'USD'): string {
  let f = MONEY.get(currency);
  if (!f) {
    f = new Intl.NumberFormat('en-US', {
      style: 'currency',
      currency,
      maximumFractionDigits: 2,
    });
    MONEY.set(currency, f);
  }
  return f.format(amount);
}

/** What a cell shows. Deliberately not a number: `metered` and `unknown` have
 *  no numeric rendering, and giving them one is the bug this module prevents. */
export function costLabel(c: AssetCost): string {
  if (c.amount === undefined) return c.raw === 'metered' ? 'metered' : c.raw;
  return (c.estimated ? '~' : '') + fmtMoney(c.amount, c.currency ?? 'USD');
}

export const BASIS_HELP: Record<CostBasis, string> = {
  measured:
    "Your actual billed amount for a completed month, read from the provider's billing API — including your negotiated discount. Historical fact, not a prediction.",
  inferred:
    "Estimated at public list price, using this resource's own attributes for the billable quantity.",
  assumed:
    'Estimated at public list price, using a default quantity from the price book because the resource did not report its own.',
  unpriceable:
    'Billing is consumption-based — requests, rows, egress. An inventory can see the resource but not the consumption, so there is no honest figure to show.',
  unknown:
    'No rule matched this resource type. That is a gap in the price book, not a free resource.',
};
