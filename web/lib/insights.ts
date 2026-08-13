import type { Asset } from './types';
import { toneColor, type Tone } from './colors';
import { fmtMoney } from './cost';

/**
 * The Insights wire contract, plus the small amount of arithmetic the summary
 * band needs.
 *
 * Hand-mirrored from internal/insight rather than generated, for the same
 * reason lib/types.ts is hand-written: the frontend must have no build-time
 * dependency on openapi.yaml. They have to be kept in step by hand.
 *
 * # The rule this module carries across the wire
 *
 * A Finding is an observation plus the ignorance that bounds it. internal/
 * insight refuses to publish one whose Caveat is empty, and nothing on this
 * side may quietly repair a report that arrives without one — see
 * {@link findingCaveat}, which exists so the card can say the caveat is missing
 * out loud rather than render a bare accusation. Dropping the finding would be
 * worse: it would hide a producer bug behind a shorter list.
 *
 * The money helpers exist for one reason too. Every *individual* figure already
 * arrives pre-rendered as a string (`Money.display`), so a card never does
 * arithmetic — it prints what the Go estimator decided to say. Only the summary
 * band's roll-up needs numbers, and everything about {@link runRate} is built
 * so that the numbers it cannot defend are counted and named rather than
 * silently treated as zero.
 */

/* ---------- wire types (mirror internal/insight) ---------- */

/** Mirrors insight.Severity. Ordered here; {@link severityRank} depends on it. */
export type Severity = 'info' | 'notable' | 'warn' | 'risk';

/** Mirrors insight.Family. A free-form slug in Go, so this is a string with
 *  the six in current use named in {@link FAMILY_ORDER}, not a closed union —
 *  a new insight can open a new section without a frontend release. */
export type Family = string;

/** Mirrors cost.Money's MarshalJSON. Every field is a *string*: the Go side
 *  serialises money as text precisely so a consumer cannot add an estimate
 *  into an invoice without noticing the tilde first. */
export interface Money {
  currency?: string;
  /** Billed figure, e.g. "1204.55". Absent when there is none. */
  measured?: string;
  /** List-price estimate, always tilde-marked, e.g. "~209.14". */
  estimated?: string;
  /** The renderer's own text — "$1,204.55 + ~$209.14", or "—". Always set. */
  display: string;
}

/** Mirrors insight.Row. */
export interface FindingRow {
  label: string;
  asset?: AssetRefLite;
  fact?: string;
  value?: string;
  money?: Money;
  related?: AssetRefLite[];
}

/** Mirrors core.AssetRef — the same shape lib/types.ts calls AssetRef, repeated
 *  here only so this module's imports say where each type comes from. */
export interface AssetRefLite {
  provider: string;
  account_id?: string;
  type: string;
  id: string;
}

/** Mirrors insight.Finding. */
export interface Finding {
  id: string;
  family: Family;
  title: string;
  summary: string;
  severity: Severity;
  count: number;
  /** What was joined to reach this. Required by the producer. */
  basis: string;
  /** What this finding cannot know. Required by the producer, and the reason
   *  this feature is trustworthy — see {@link findingCaveat}. */
  caveat: string;
  rows?: FindingRow[];
  total?: Money;
}

/** Mirrors insight.Scope — what the run had to work with. Every field is here
 *  because its absence is a plausible explanation for a thin report. */
export interface InsightScope {
  assets: number;
  types: number;
  providers: string[];
  edges: number;
  raw_assets: number;
  priced: boolean;
}

/** Mirrors one entry of insight.Report.Skipped. An insight that could not look
 *  must never be indistinguishable from one that looked and found nothing, so
 *  these are rendered, not dropped. */
export interface SkippedInsight {
  insight: string;
  title?: string;
  family?: Family;
  reason: string;
}

/**
 * Mirrors insight.Suppressed — a finding the framework produced but refused to
 * publish, because it did not meet the contract every finding must meet (most
 * often: no caveat).
 *
 * Rendered as loudly as a finding, matching the CLI's "REFUSED" block. This is
 * a bug in an insight rather than a property of the estate, and burying it
 * would leave the reader with a silently shorter report — the one failure this
 * whole feature is arranged to prevent.
 */
export interface SuppressedFinding {
  insight: string;
  finding?: string;
  title?: string;
  reason: string;
}

/**
 * Mirrors insight.Report.
 *
 * Only `findings` is treated as load-bearing. Everything else is optional on
 * this side even where Go always emits it, because a report that arrives from
 * an older or newer binary should render the findings it has rather than blank
 * the page over a field the UI merely wanted.
 */
export interface InsightReport {
  findings: Finding[];
  /** The standing "an inventory cannot see…" text. Rendered verbatim. */
  disclaimer?: string;
  /** False when the run was cancelled or cut short. */
  complete?: boolean;
  scope?: InsightScope;
  /** Run-level qualifications — "Only one provider", "Cost estimation is off". */
  notes?: string[];
  skipped?: SkippedInsight[];
  suppressed?: SuppressedFinding[];
  /** Findings dropped by a min-severity filter. Non-zero means this report is
   *  quieter than the run was, which the reader has to be told. */
  hidden?: number;
  generated_at?: string;
  init_errors?: string[];
  errors?: string[];
}

/* ---------- severity ---------- */

/** Ascending, matching insight.severityRank. */
export const SEVERITIES: readonly Severity[] = ['risk', 'warn', 'notable', 'info'];

const SEVERITY_RANK: Record<Severity, number> = { info: 0, notable: 1, warn: 2, risk: 3 };

export function severityRank(s: Severity): number {
  return SEVERITY_RANK[s] ?? -1;
}

/**
 * The tone a severity paints in.
 *
 * `info` maps to neutral rather than to the info blue: it "describes the
 * estate, nothing to do", and a page where the harmless findings are the most
 * saturated thing on screen teaches the reader to ignore colour entirely.
 */
export function severityTone(s: Severity): Tone {
  switch (s) {
    case 'risk':
      return 'err';
    case 'warn':
      return 'warn';
    case 'notable':
      return 'info';
    default:
      return 'neutral';
  }
}

export function severityColor(s: Severity): string {
  return toneColor(severityTone(s));
}

/**
 * What each severity means, quoted from the Go doc comments so the CLI's
 * `--min-severity` and this page agree about what a word buys you.
 *
 * The framing matters more than the individual lines: severity ranks *the
 * question*, not the resource. This tool can see that a bucket is public; it
 * cannot see whether the bucket is meant to be public, and the caveat on the
 * finding is what says so.
 */
export const SEVERITY_HELP: Record<Severity, string> = {
  risk: 'The evidence is exact and the consequence is a security or availability incident. Rare on purpose — a report where everything is a risk is a report nobody reads twice.',
  warn: 'Probably unintended, and cheap to check.',
  notable: 'A pattern worth knowing about that may well be deliberate — an unusual shape, not a defect.',
  info: 'Describes the estate. Nothing to do; useful for orientation.',
};

/* ---------- family ---------- */

/** Mirrors insight.familyOrder: the consequential sections lead. A family that
 *  is not listed sorts after every family that is, alphabetically — so an
 *  insight can invent one without a frontend change. */
export const FAMILY_ORDER: readonly Family[] = [
  'exposure',
  'access',
  'network',
  'resilience',
  'cost',
  'hygiene',
];

export function familyRank(f: Family): number {
  const i = FAMILY_ORDER.indexOf(f);
  return i === -1 ? FAMILY_ORDER.length : i;
}

/**
 * A family's heading text.
 *
 * Sentence case in the DOM, upper-cased by CSS — unlike Go's `Family.Title()`,
 * which upper-cases the string itself. A screen reader given "EXPOSURE" may
 * spell it out letter by letter; `text-transform` leaves the accessible name
 * alone and changes only the pixels.
 */
export function familyLabel(f: Family): string {
  const words = f.replace(/[-_]/g, ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** One line per family, so a section heading says what it collects. Unknown
 *  families get no line rather than a guessed one. */
export const FAMILY_HELP: Record<string, string> = {
  exposure: 'What is reachable from outside.',
  access: 'Who, or what, may act.',
  network: 'How traffic is arranged.',
  resilience: 'What breaks when one thing does.',
  cost: 'Money-shaped questions.',
  hygiene: 'Leftovers, drift, and missing metadata.',
};

/* ---------- the caveat contract ---------- */

/**
 * The caveat to render, and whether the producer supplied one.
 *
 * Never returns an empty string. A finding that arrives without a caveat is a
 * bug in whatever produced it — internal/insight's `ValidateFinding` refuses to
 * publish one — and the honest response is to render the finding *with the
 * defect named*, not to hide the finding and not to invent a reassuring
 * sentence on the producer's behalf.
 */
export function findingCaveat(f: Finding): { text: string; supplied: boolean } {
  const text = (f.caveat ?? '').trim();
  if (text !== '') return { text, supplied: true };
  return {
    supplied: false,
    text: 'This finding arrived without a caveat, which every finding is required to carry. Treat it as unqualified: nothing here states what it could not see.',
  };
}

/* ---------- money ---------- */

/** What an aggregate with nothing in it renders as, matching cost.NoMoney. An
 *  em dash cannot be misread as a figure the way "0.00" can. */
export const NO_MONEY = '—';

/**
 * Reads one of Money's string components as a number.
 *
 * Deliberately strict. The Go formatter emits `"<0.0001"` for "a positive
 * amount too small to print", and `Number("<0.0001")` is NaN — but a NaN that
 * reached a sum would poison a whole currency's total, and a NaN coerced to 0
 * would understate it. So anything that is not plainly a figure comes back as
 * `unparsed`, and {@link runRate} counts those separately and says so.
 */
function readAmount(v: string | undefined): { n: number; unparsed: boolean } {
  if (v === undefined || v.trim() === '') return { n: 0, unparsed: false };
  const bare = v.trim().replace(/^~/, '');
  if (!/^\d/.test(bare)) return { n: 0, unparsed: true };
  const n = Number(bare.replace(/,/g, ''));
  return Number.isFinite(n) ? { n, unparsed: false } : { n: 0, unparsed: true };
}

/**
 * Formats a figure, mirroring cost.Money.String() so the CLI and this page
 * render the same total the same way: the tilde marks the estimated half only,
 * and the two halves are never collapsed into one number.
 *
 * `fmtMoney` goes through Intl, which throws on a currency code it does not
 * recognise — including the empty string, which cost.Money legitimately carries
 * when nothing named a currency. So an unrecognised code is prefixed as text
 * instead, the same fallback cost.currencySymbol makes.
 */
function money(amount: number, currency: string): string {
  if (/^[A-Za-z]{3}$/.test(currency)) return fmtMoney(amount, currency.toUpperCase());
  const n = amount.toFixed(2);
  return currency === '' ? n : `${currency} ${n}`;
}

function figure(currency: string, measured: number, estimated: number): string {
  const m = measured > 0 ? money(measured, currency) : '';
  const e = estimated > 0 ? `~${money(estimated, currency)}` : '';
  if (m !== '' && e !== '') return `${m} + ${e}`;
  return m !== '' ? m : e !== '' ? e : NO_MONEY;
}

/** One currency's share of the roll-up. Currencies are never combined: no
 *  exchange rate exists anywhere in this tool, so a mixed total is not a
 *  number. Mirrors cost.moneyBag and insight.SumMoney's refusal. */
export interface RunRateLine {
  currency: string;
  measured: number;
  estimated: number;
  /** "$1,204.55 + ~$209.14" */
  monthly: string;
  /** The same, times twelve. */
  yearly: string;
}

export interface RunRate {
  lines: RunRateLine[];
  /** How many finding totals were folded in. */
  findings: number;
  /** Figures that were present but not summable — see {@link readAmount}. */
  unparsed: number;
}

/**
 * Rolls every finding's Total up into a monthly and yearly run rate.
 *
 * Two things this deliberately does not do, both of which the band's caveat
 * has to state because no amount of care here can fix them:
 *
 *  - It does not de-duplicate. Two cost findings can legitimately count the
 *    same volume ("detached" and "unattached in a stopped compartment"), and
 *    nothing in the wire format says which assets a Total covered. The sum is
 *    therefore an upper bound across findings, not an estate total — the Cost
 *    page's own roll-up is the number for that.
 *  - It does not annualise anything but arithmetic. ×12 is a *run rate*: what
 *    the current shape costs if it neither grows nor shrinks and nothing is
 *    billed by consumption. It is not a forecast and must not be labelled one.
 */
export function runRate(findings: readonly Finding[]): RunRate | null {
  const acc = new Map<string, { measured: number; estimated: number }>();
  let contributing = 0;
  let unparsed = 0;

  for (const f of findings) {
    const t = f.total;
    if (!t) continue;
    const m = readAmount(t.measured);
    const e = readAmount(t.estimated);
    if (m.unparsed) unparsed += 1;
    if (e.unparsed) unparsed += 1;
    if (m.n === 0 && e.n === 0) {
      // A total that is entirely unsummable still counts as a finding that
      // wanted to contribute — otherwise "3 findings" would silently become
      // "1 finding" and the reader would never learn two figures went missing.
      if (m.unparsed || e.unparsed) contributing += 1;
      continue;
    }
    const cur = t.currency ?? '';
    const bucket = acc.get(cur) ?? { measured: 0, estimated: 0 };
    bucket.measured += m.n;
    bucket.estimated += e.n;
    acc.set(cur, bucket);
    contributing += 1;
  }

  if (contributing === 0) return null;

  const lines = [...acc.entries()]
    .sort((a, b) => a[0].localeCompare(b[0]))
    .map(([currency, v]) => ({
      currency,
      measured: v.measured,
      estimated: v.estimated,
      monthly: figure(currency, v.measured, v.estimated),
      yearly: figure(currency, v.measured * 12, v.estimated * 12),
    }));

  return { lines, findings: contributing, unparsed };
}

/* ---------- public addresses ---------- */

/**
 * The finding that counts publicly-reachable addresses, if the run produced
 * one.
 *
 * Resolved by id rather than recomputed here, and returning `null` — never
 * zero — when no such finding exists. That distinction is the whole point: an
 * inventory where the exposure insight never ran and an inventory with nothing
 * public would otherwise render as the same confident "0", and only one of
 * those is a statement anyone should act on.
 *
 * The id list is checked before the pattern so the two spellings already in the
 * Go tree resolve exactly; the pattern is what keeps a renamed or newly added
 * insight from silently emptying this tile.
 */
const PUBLIC_ADDRESS_IDS: readonly string[] = [
  'exposure.public-endpoints',
  'network.public-endpoints',
  'exposure.public-addresses',
];

const PUBLIC_ADDRESS_RE = /(^|[.-])public-(endpoints|addresses|ips|addr)$/;

export function publicAddressFinding(findings: readonly Finding[]): Finding | null {
  for (const id of PUBLIC_ADDRESS_IDS) {
    const hit = findings.find((f) => f.id === id);
    if (hit) return hit;
  }
  return findings.find((f) => PUBLIC_ADDRESS_RE.test(f.id)) ?? null;
}

/* ---------- counts ---------- */

export function countBySeverity(findings: readonly Finding[]): Record<Severity, number> {
  const out: Record<Severity, number> = { risk: 0, warn: 0, notable: 0, info: 0 };
  for (const f of findings) {
    if (f.severity in out) out[f.severity] += 1;
  }
  return out;
}

/** Families present, in report order. */
export function familiesOf(findings: readonly Finding[]): Family[] {
  const seen = new Set<Family>();
  for (const f of findings) seen.add(f.family);
  return [...seen].sort((a, b) => familyRank(a) - familyRank(b) || a.localeCompare(b));
}

/* ---------- transport ---------- */

/** Same-origin, like every other call in this app — the Go binary serves both
 *  halves, which is what lets one build work behind a port-forward or an
 *  Ingress sub-path. Declared here rather than imported from lib/api.ts only
 *  because this module owns the whole insights surface: types, roll-up and the
 *  one request. Move it beside buildTopology/buildReach the moment a second
 *  page needs it. */
const ENDPOINT = '/api/v1/insights';

/**
 * Derives findings from assets the browser already holds.
 *
 * A POST for the same reason the Topology and Exposure pages POST: the audit is
 * already in memory, and re-running every provider to answer a question about
 * data we have would spend the operator's API quota twice. It also means this
 * page works against a run that has since been stopped.
 *
 * The include-raw caveat that applies to those two applies here as well — the
 * SSE stream only carries `raw` when the server was started with
 * `--include-raw`, and any insight that parses a payload is inert without it.
 * The report says so itself, through `scope.raw_assets` and its own notes;
 * the page renders both rather than restating them from this side.
 */
export async function buildInsights(
  assets: readonly Asset[],
  signal?: AbortSignal,
): Promise<InsightReport> {
  const res = await fetch(ENDPOINT, {
    method: 'POST',
    signal,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ assets }),
  });
  if (!res.ok) {
    // The server explains an empty inventory and a malformed body in the text;
    // the status alone would hide the actionable half.
    throw new Error((await res.text()).trim() || `insights → ${res.status} ${res.statusText}`);
  }
  return normalizeReport(await res.json());
}

/**
 * Coerces whatever the endpoint returned into a report.
 *
 * Tolerant on purpose. A bare `Finding[]` and a `{findings: […]}` envelope are
 * both plausible shapes for this endpoint to settle on — `/api/v1/topology`
 * already accepts both forms on the way *in* — and the page should render a
 * list of findings either way rather than fail on the wrapper.
 *
 * What it does not do is invent structure: a payload with no recognisable
 * findings array yields an empty list, and the page's empty state says the run
 * produced nothing rather than pretending the estate is clean.
 */
export function normalizeReport(raw: unknown): InsightReport {
  if (Array.isArray(raw)) return { findings: raw as Finding[] };
  if (raw && typeof raw === 'object') {
    const obj = raw as Partial<InsightReport>;
    return { ...obj, findings: Array.isArray(obj.findings) ? obj.findings : [] };
  }
  return { findings: [] };
}

/* ---------- markdown ---------- */

/**
 * Renders the report the browser is holding as Markdown, for pasting into a
 * ticket or an incident channel.
 *
 * A client-side copy rather than a download link to the server's own markdown
 * renderer, because that route is a GET: it would re-run every provider to
 * re-derive a report already on screen, and take as long as an audit to hand
 * back something the reader has already read.
 *
 * The layout mirrors internal/insight's markdown renderer — family headings,
 * and the caveat as a blockquote *above* the detail table — so a report pasted
 * from the UI and one piped from the CLI are the same document. If that
 * renderer moves, this should move with it.
 */
export function toMarkdown(report: InsightReport, findings: readonly Finding[]): string {
  const out: string[] = ['# Insights', ''];

  if (report.scope) {
    const s = report.scope;
    out.push(
      `${s.assets} assets · ${s.types} types · ${s.providers.join(', ') || 'no providers'} · ${s.edges} edges` +
        (s.raw_assets === 0 ? ' · no raw payloads' : '') +
        (s.priced ? '' : ' · cost estimation off'),
      '',
    );
  }
  if (report.complete === false) {
    out.push('> **Partial report** — the run did not finish, so findings may be missing.', '');
  }

  for (const fam of familiesOf(findings)) {
    out.push(`### ${fam.toUpperCase()}`, '');
    for (const f of findings.filter((x) => x.family === fam)) {
      const caveat = findingCaveat(f);
      out.push(`#### ${f.title}`, '');
      out.push(`${f.summary}`, '');
      out.push(`**Basis** — ${f.basis}`, '');
      out.push(`> **Cannot know** — ${caveat.text}`, '');
      if (f.total) out.push(`**Total** — ${f.total.display}`, '');
      if (f.rows && f.rows.length > 0) {
        out.push('| | Observation | Value |', '| --- | --- | --- |');
        for (const r of f.rows) {
          out.push(
            `| ${mdCell(r.label)} | ${mdCell(r.fact ?? '')} | ${mdCell(r.money?.display ?? r.value ?? '')} |`,
          );
        }
        out.push('');
      }
    }
  }

  if (report.suppressed && report.suppressed.length > 0) {
    out.push('### REFUSED', '');
    out.push(
      'These findings were produced but not published, because they do not meet the contract every finding in this tool must meet. This is a bug in the insight, not a property of the estate.',
      '',
    );
    for (const s of report.suppressed) {
      out.push(`- **${s.insight}**${s.finding ? ` (${s.finding})` : ''} — ${s.reason}`);
    }
    out.push('');
  }
  if (report.skipped && report.skipped.length > 0) {
    out.push('### NOT RUN', '');
    for (const s of report.skipped) out.push(`- **${s.insight}** — ${s.reason}`);
    out.push('');
  }
  if (report.hidden && report.hidden > 0) {
    out.push(`_${report.hidden} finding(s) hidden by the severity filter._`, '');
  }
  if (report.notes && report.notes.length > 0) {
    out.push('---', '');
    for (const n of report.notes) out.push(`- ${n}`);
    out.push('');
  }
  if (report.disclaimer) out.push('---', '', report.disclaimer, '');

  return out.join('\n');
}

/** Escapes a pipe so one cell cannot silently become two columns. */
function mdCell(s: string): string {
  return s.replace(/\|/g, '\\|').replace(/\n/g, ' ');
}
