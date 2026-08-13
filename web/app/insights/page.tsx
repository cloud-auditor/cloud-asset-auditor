'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useAudit } from '@/components/AuditProvider';
import { AuditControls } from '@/components/AuditControls';
import { FindingCard } from '@/components/FindingCard';
import { StatTile } from '@/components/StatTile';
import { copyText } from '@/components/JsonView';
import { vars } from '@/lib/css';
import { fmtCount } from '@/lib/format';
import { Icon } from '@/lib/icons';
import {
  FAMILY_HELP,
  NO_MONEY,
  SEVERITIES,
  SEVERITY_HELP,
  buildInsights,
  countBySeverity,
  familiesOf,
  familyLabel,
  familyRank,
  publicAddressFinding,
  runRate,
  severityColor,
  severityRank,
  toMarkdown,
  type Family,
  type Finding,
  type InsightReport,
  type Severity,
} from '@/lib/insights';
import './insights.css';

/**
 * An empty selection means "everything", the same encoding the Topology page's
 * edge-kind filter and the audit scope picker both use — and it carries the
 * same trap. Without materialising the full set on the first click, clicking a
 * lit chip would *isolate* that value rather than switch it off, which is the
 * opposite of what a pressed toggle promises. A click that would empty the
 * selection is refused (the encoding has no way to spell "none", and an empty
 * list is not a state anyone asked for), and a set grown to cover everything
 * collapses back to empty so there is exactly one representation of "all".
 */
function toggle<T>(prev: Set<T>, key: T, all: readonly T[]): Set<T> {
  const next = prev.size === 0 ? new Set<T>(all) : new Set<T>(prev);
  if (next.has(key)) next.delete(key);
  else next.add(key);
  if (next.size === 0) return prev;
  return next.size === all.length ? new Set<T>() : next;
}

export default function InsightsPage() {
  const { assets, running, toast } = useAudit();

  const [report, setReport] = useState<InsightReport | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [families, setFamilies] = useState<Set<Family>>(new Set());
  const [severities, setSeverities] = useState<Set<Severity>>(new Set());

  const derive = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const next = await buildInsights(assets);
      setReport(next);
      // A new report is a new set of families and severities; carrying the
      // previous filter over would silently hide sections the reader has
      // never seen, which on this page means hiding findings.
      setFamilies(new Set());
      setSeverities(new Set());
    } catch (e) {
      setReport(null);
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [assets]);

  // `?derive=1` produces a finished report with no interaction — the
  // counterpart to `?run=1` on the audit, `?build=1` on the topology and
  // `?trace=1` on exposure, and what a kiosk display or a screenshot pipeline
  // needs. It waits for the run to END for the reason those two do: findings
  // derived from a third of the inventory are not an early version of the real
  // ones, they are different ones — and here the difference runs in the
  // dangerous direction, because a half-collected estate produces *fewer*
  // findings and reads as a cleaner one.
  const autoDerived = useRef(false);
  useEffect(() => {
    if (autoDerived.current || running || assets.length === 0) return;
    const q = new URLSearchParams(window.location.search);
    if (q.get('derive') !== '1' && q.get('derive') !== 'true') return;
    autoDerived.current = true;
    void derive();
  }, [running, assets.length, derive]);

  const findings = useMemo(() => report?.findings ?? [], [report]);

  // Everything in the band is computed over the whole report, never over the
  // filtered subset: it summarises what was found, and a run rate that shrank
  // when the reader narrowed to one family would be a different number wearing
  // the same label.
  const bySeverity = useMemo(() => countBySeverity(findings), [findings]);
  const presentSeverities = useMemo(
    () => SEVERITIES.filter((s) => bySeverity[s] > 0),
    [bySeverity],
  );
  const allFamilies = useMemo(() => familiesOf(findings), [findings]);
  const rate = useMemo(() => runRate(findings), [findings]);
  const publicAddresses = useMemo(() => publicAddressFinding(findings), [findings]);

  const visible = useMemo(
    () =>
      findings.filter(
        (f) =>
          (families.size === 0 || families.has(f.family)) &&
          (severities.size === 0 || severities.has(f.severity)),
      ),
    [findings, families, severities],
  );

  const grouped = useMemo(() => {
    const out = new Map<Family, Finding[]>();
    for (const f of visible) {
      const list = out.get(f.family);
      if (list) list.push(f);
      else out.set(f.family, [f]);
    }
    // Within a family the most consequential finding leads; ties keep the
    // report's own order, which internal/insight already made deterministic.
    for (const list of out.values()) {
      list.sort((a, b) => severityRank(b.severity) - severityRank(a.severity));
    }
    return [...out.entries()].sort(
      ([a], [b]) => familyRank(a) - familyRank(b) || a.localeCompare(b),
    );
  }, [visible]);

  const filtered = visible.length !== findings.length;

  const copyMarkdown = useCallback(async () => {
    if (!report) return;
    const ok = await copyText(toMarkdown(report, visible));
    toast(
      ok
        ? {
            kind: 'ok',
            title: 'Copied as Markdown',
            body: `${fmtCount(visible.length)} finding${visible.length === 1 ? '' : 's'}, each with its caveat`,
          }
        : {
            kind: 'warn',
            title: 'Could not copy',
            body: 'The clipboard is unavailable on this origin.',
          },
    );
  }, [report, visible, toast]);

  const resultErrors = (report?.init_errors ?? []).concat(report?.errors ?? []);

  // Phrased once rather than branched in the markup: the sentence is the same
  // claim before and after a run, and only the quantity is unknown up front.
  const streamed =
    assets.length === 0
      ? 'the assets streamed to this browser'
      : `the ${fmtCount(assets.length)} asset${assets.length === 1 ? '' : 's'} already streamed to this browser`;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Insights</h1>
          <p>
            What the inventory already collected, read back as findings — each one carrying the
            question it cannot answer.
          </p>
        </div>
        <div className="spacer" />
        {report && findings.length > 0 && (
          <button type="button" className="btn" onClick={copyMarkdown}>
            <Icon name="copy" size={13} />
            Copy as Markdown
          </button>
        )}
      </div>

      <AuditControls />

      <div className="card ins-run">
        <div className="card-body ins-run-body">
          <button
            className="primary"
            type="button"
            disabled={assets.length === 0 || loading || running}
            onClick={() => void derive()}
          >
            <Icon name="search" size={13} />
            {loading ? 'Deriving…' : report ? 'Re-derive' : 'Derive insights'}
          </button>
          <p className="hint ins-run-hint">
            Derived from {streamed} and the topology graph inferred from them — no provider is
            queried and no new data is collected. Nothing below can see more than the audit above
            did.
          </p>
        </div>
      </div>

      {error && (
        <div className="banner error">
          <Icon name="alert" size={15} />
          <span>{error}</span>
        </div>
      )}

      {report?.complete === false && (
        <div className="banner warn">
          <Icon name="alert" size={15} />
          <span>
            Partial report — the run did not finish, so findings are missing rather than absent.
          </span>
        </div>
      )}

      {resultErrors.length > 0 && (
        <div className="banner warn">
          <Icon name="alert" size={15} />
          <span>
            {resultErrors.length} provider error{resultErrors.length === 1 ? '' : 's'} while
            deriving — everything below is drawn from an incomplete inventory.
            <span className="mono faint" style={{ display: 'block', marginTop: 4 }}>
              {resultErrors[0]}
            </span>
          </span>
        </div>
      )}

      {!report ? (
        <div className="card">
          <div className="empty">
            <Icon name="search" size={30} strokeWidth={1.3} />
            <h3>{assets.length === 0 ? 'Nothing to derive from yet' : 'Ready to derive'}</h3>
            <p>
              {assets.length === 0
                ? 'Findings are derived from a collected inventory. Run an audit above, then derive.'
                : 'Press Derive insights. Everything is computed from the assets already in the browser, so it costs no API calls.'}
            </p>
          </div>
        </div>
      ) : (
        <>
          {/* A band of zeros above an empty report says nothing the panel below
              does not say better, so the summary collapses to the one part that
              still carries information: what the run was able to look at. */}
          {findings.length > 0 ? (
            <Band
              report={report}
              findings={findings}
              bySeverity={bySeverity}
              presentSeverities={presentSeverities}
              allFamilies={allFamilies}
              families={families}
              severities={severities}
              onToggleFamily={(f) => setFamilies((prev) => toggle(prev, f, allFamilies))}
              onToggleSeverity={(s) => setSeverities((prev) => toggle(prev, s, presentSeverities))}
              onReset={() => {
                setFamilies(new Set());
                setSeverities(new Set());
              }}
              rate={rate}
              publicAddresses={publicAddresses}
              visibleCount={visible.length}
              filtered={filtered}
            />
          ) : (
            report.scope && (
              <div className="card ins-band">
                <div className="card-body ins-band-body">
                  <ScopeStrip scope={report.scope} />
                </div>
              </div>
            )
          )}

          {report.notes && report.notes.length > 0 && <Notes notes={report.notes} />}

          {/* Loudest of the three panels, and above them: a refusal means a
              finding was produced and withheld, so this report is shorter than
              the run was and the shortfall is a defect rather than a fact
              about the estate. */}
          {report.suppressed && report.suppressed.length > 0 && (
            <Suppressed suppressed={report.suppressed} />
          )}

          {report.skipped && report.skipped.length > 0 && <Skipped skipped={report.skipped} />}

          {report.hidden != null && report.hidden > 0 && (
            <div className="banner info">
              <Icon name="filter" size={15} />
              <span>
                {fmtCount(report.hidden)} finding{report.hidden === 1 ? '' : 's'} hidden by the
                server&apos;s severity filter — this report is quieter than the run was.
              </span>
            </div>
          )}

          {findings.length === 0 ? (
            <NoFindings />
          ) : visible.length === 0 ? (
            <div className="card">
              <div className="empty">
                <Icon name="filter" size={28} strokeWidth={1.3} />
                <h3>No finding matches this filter</h3>
                <p>
                  {fmtCount(findings.length)} finding{findings.length === 1 ? '' : 's'} were derived.
                  Widen the filter above to see them.
                </p>
              </div>
            </div>
          ) : (
            grouped.map(([family, list]) => (
              <section key={family} className="ins-family">
                <div className="ins-family-head">
                  <h2>{familyLabel(family)}</h2>
                  {FAMILY_HELP[family] && (
                    <span className="ins-family-help">{FAMILY_HELP[family]}</span>
                  )}
                  <span className="spacer" />
                  <span className="count-badge">{fmtCount(list.length)}</span>
                </div>
                <div className="ins-cards">
                  {list.map((f, i) => (
                    <FindingCard key={`${f.id}:${i}`} finding={f} />
                  ))}
                </div>
              </section>
            ))
          )}

          {/* The standing frame, last. It is the same paragraph on every run,
              and it is not what qualifies an individual finding — each card
              carries its own caveat for that, which is the whole reason this
              one can sit below the content instead of gatekeeping it. */}
          {report.disclaimer && (
            <div
              className="card surface-rail ins-disclaimer"
              style={vars({ '--rail': 'var(--info)' })}
            >
              <div className="card-body">
                <h2 className="ins-panel-title">What an inventory cannot see</h2>
                <p>{report.disclaimer}</p>
              </div>
            </div>
          )}
        </>
      )}
    </>
  );
}

/* ---------- summary band ---------- */

interface BandProps {
  report: InsightReport;
  findings: Finding[];
  bySeverity: Record<Severity, number>;
  presentSeverities: Severity[];
  allFamilies: Family[];
  families: Set<Family>;
  severities: Set<Severity>;
  onToggleFamily: (f: Family) => void;
  onToggleSeverity: (s: Severity) => void;
  onReset: () => void;
  rate: ReturnType<typeof runRate>;
  publicAddresses: Finding | null;
  visibleCount: number;
  filtered: boolean;
}

/**
 * The band above the findings: the shape of the report, then the controls that
 * narrow it.
 *
 * Every number here is a *finding's own* count restated at a larger size, which
 * is exactly how a summary strips the qualification off a claim. The band
 * therefore carries its own caveat line — the run rate says what its arithmetic
 * cannot settle, and the tiles say out loud that what each number cannot know
 * is on the card below. Without that line this band would be the one place on
 * the page that states more than it knows.
 */
function Band({
  report,
  findings,
  bySeverity,
  presentSeverities,
  allFamilies,
  families,
  severities,
  onToggleFamily,
  onToggleSeverity,
  onReset,
  rate,
  publicAddresses,
  visibleCount,
  filtered,
}: BandProps) {
  const scope = report.scope;

  return (
    <div className="card ins-band">
      <div className="card-body ins-band-body">
        <div className="stats stagger">
          <StatTile
            icon="layers"
            label="Findings"
            value={findings.length}
            sub={
              allFamilies.length > 0
                ? `across ${allFamilies.length} famil${allFamilies.length === 1 ? 'y' : 'ies'}`
                : undefined
            }
          />

          {/* Never a bare zero. A run whose exposure insight did not produce a
              count and an estate with nothing public are different facts, and
              "0" would render them identically — see publicAddressFinding. */}
          <StatTile
            icon="globe"
            label="Public addresses"
            value={publicAddresses ? publicAddresses.count : NO_MONEY}
            color={
              publicAddresses
                ? severityColor(publicAddresses.severity)
                : 'var(--text-faint)'
            }
            sub={publicAddresses ? `from “${publicAddresses.title}”` : 'no finding reported one'}
            title={
              publicAddresses
                ? undefined
                : 'Not the same as zero: no insight in this run counted publicly reachable addresses.'
            }
          />

          {rate?.lines.map((line) => (
            <MoneyTile
              key={`m:${line.currency}`}
              label={rate.lines.length > 1 ? `Monthly (${line.currency})` : 'Monthly run rate'}
              figure={line.monthly}
              sub={line.measured > 0 && line.estimated > 0 ? 'billed + estimated' : undefined}
            />
          ))}
          {rate?.lines.map((line) => (
            <MoneyTile
              key={`y:${line.currency}`}
              label={rate.lines.length > 1 ? `Yearly (${line.currency})` : 'Yearly run rate'}
              figure={line.yearly}
              sub="12 × monthly"
            />
          ))}
        </div>

        {scope && <ScopeStrip scope={scope} />}

        <div className="ins-filters">
          <FilterGroup label="Severity">
            {presentSeverities.map((s) => {
              const on = severities.size === 0 || severities.has(s);
              return (
                <button
                  key={s}
                  type="button"
                  className={`chip${on ? ' on' : ''}`}
                  style={on ? { color: severityColor(s), borderColor: severityColor(s) } : undefined}
                  aria-pressed={on}
                  title={SEVERITY_HELP[s]}
                  onClick={() => onToggleSeverity(s)}
                >
                  <span className="dot" style={{ background: severityColor(s) }} />
                  {s}
                  <span className="count-badge">{fmtCount(bySeverity[s])}</span>
                </button>
              );
            })}
          </FilterGroup>

          <FilterGroup label="Family">
            {allFamilies.map((f) => {
              const on = families.size === 0 || families.has(f);
              const n = findings.filter((x) => x.family === f).length;
              return (
                <button
                  key={f}
                  type="button"
                  className={`chip${on ? ' on' : ''}`}
                  aria-pressed={on}
                  title={FAMILY_HELP[f]}
                  onClick={() => onToggleFamily(f)}
                >
                  {familyLabel(f)}
                  <span className="count-badge">{fmtCount(n)}</span>
                </button>
              );
            })}
          </FilterGroup>

          <span className="spacer" />
          {filtered && (
            <>
              <span className="hint">
                showing {fmtCount(visibleCount)} of {fmtCount(findings.length)}
              </span>
              <button type="button" className="btn ghost sm" onClick={onReset}>
                Reset
              </button>
            </>
          )}
        </div>

        {/* The rule belongs to the wrapper, not to the paragraph: a border-top
            on an element that caps its own measure stops where the text stops,
            and a divider that quits two-thirds across reads as a fault. */}
        <div className="ins-band-foot">
          <p className="ins-band-caveat">
            <strong>What these numbers do not settle.</strong> Each one is a finding&apos;s own
            count, restated larger; what that finding could not see is written on its card below,
            and the count means nothing without it.
            {rate && (
              <>
                {' '}
                The run rate adds up {fmtCount(rate.findings)} finding total
                {rate.findings === 1 ? '' : 's'}, so a resource that two findings both count is
                counted twice here — it is an upper bound across findings, not an estate bill. The
                yearly figure is arithmetic, not a forecast: it assumes the estate does not change,
                and it excludes every resource billed by consumption, because an inventory can see
                the resource but never the consumption.
                {rate.unparsed > 0 && (
                  <>
                    {' '}
                    {fmtCount(rate.unparsed)} figure{rate.unparsed === 1 ? '' : 's'} were too small
                    to print as a number and are left out of the sums rather than counted as zero.
                  </>
                )}
              </>
            )}
          </p>
        </div>
      </div>
    </div>
  );
}

/**
 * A money tile, sharing `.card.stat`'s vocabulary but not StatTile itself.
 *
 * StatTile reserves the value's width in `ch` so a number counting up cannot
 * nudge its neighbours — a guard that buys nothing here, because a string value
 * never animates, and one that actively hurts: "$1,204.55 + ~$622.04" reserves
 * more width than a tile gets in a four-column grid on a small laptop, and
 * `.stat`'s `overflow: hidden` would then crop it to a *smaller, plausible*
 * figure with nothing to say it had been cropped. Wrapping is the only safe
 * failure mode for a number, so this one wraps.
 */
function MoneyTile({ label, figure, sub }: { label: string; figure: string; sub?: string }) {
  return (
    <div className="card stat">
      <div className="k">
        <Icon name="cloud" size={13} />
        {label}
      </div>
      <div className="v ins-money">{figure}</div>
      {sub && <div className="sub">{sub}</div>}
    </div>
  );
}

function FilterGroup({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div className="ins-filter-group" role="group" aria-label={label}>
      <span className="ins-filter-label">{label}</span>
      {children}
    </div>
  );
}

/**
 * What this run could see, in one line.
 *
 * The two absences are tinted because they are the usual explanations for a
 * thin report, and a reader who does not know the payloads were missing will
 * read the short list of findings as good news.
 */
function ScopeStrip({ scope }: { scope: NonNullable<InsightReport['scope']> }) {
  return (
    <div className="ins-scope">
      <span className="ins-scope-label">Derived from</span>
      <span className="chip">{fmtCount(scope.assets)} assets</span>
      <span className="chip">{fmtCount(scope.types)} types</span>
      <span className="chip">
        {scope.providers.length === 0 ? 'no providers' : scope.providers.join(', ')}
      </span>
      <span className="chip">{fmtCount(scope.edges)} edges</span>
      <span className={`chip${scope.raw_assets === 0 ? ' ins-gap' : ''}`}>
        {scope.raw_assets === 0
          ? 'no raw payloads — every insight that reads one was inert'
          : `${fmtCount(scope.raw_assets)} with raw payloads`}
      </span>
      <span className={`chip${scope.priced ? '' : ' ins-gap'}`}>
        {scope.priced ? 'cost estimated' : 'cost estimation off — no figures'}
      </span>
    </div>
  );
}

/* ---------- run-level qualifications ---------- */

/** The report's own notes about its scope. Rendered as the producer wrote
 *  them: they are already phrased as qualifications, and paraphrasing a
 *  qualification is how it becomes a reassurance. */
function Notes({ notes }: { notes: string[] }) {
  return (
    <div className="card surface-rail ins-notes" style={vars({ '--rail': 'var(--warn)' })}>
      <div className="card-body">
        <h2 className="ins-panel-title">About this report</h2>
        <ul>
          {notes.map((n, i) => (
            <li key={i}>{n}</li>
          ))}
        </ul>
      </div>
    </div>
  );
}

/**
 * Findings the framework produced and refused to publish.
 *
 * The CLI prints this as a "REFUSED" block and calls it a bug to report; the
 * page says the same thing in the same words, in danger colours, above the
 * findings. The reason it earns that weight: a suppressed finding is one the
 * reader would otherwise never learn existed, and the most common cause of
 * suppression is precisely a missing caveat — the contract this whole page is
 * built to keep. Rendering it quietly would be the framework enforcing the rule
 * and the UI hiding the enforcement.
 */
function Suppressed({ suppressed }: { suppressed: NonNullable<InsightReport['suppressed']> }) {
  return (
    <div className="card surface-rail ins-suppressed" style={vars({ '--rail': 'var(--danger)' })}>
      <div className="card-body">
        <h2 className="ins-panel-title">
          <Icon name="alert" size={15} />
          Refused
          <span className="count-badge">{fmtCount(suppressed.length)}</span>
        </h2>
        <p className="hint">
          These findings were produced but not published, because they do not meet the contract
          every finding in this tool must meet — most often, they named no caveat. This is a bug in
          the insight, not a property of your estate. Please report it.
        </p>
        <ul className="ins-skipped-list">
          {suppressed.map((s, i) => (
            <li key={`${s.insight}:${i}`}>
              <code>{s.finding || s.insight}</code>
              <span>{s.reason}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

/**
 * Insights that did not run.
 *
 * Prominent, and never collapsed by default. An insight that could not look
 * produces zero findings, which on a page of findings is indistinguishable
 * from an insight that looked and found nothing — and the second reads as good
 * news while the first is a gap in the audit. internal/insight goes to the
 * trouble of skipping-with-a-reason instead of falling silent precisely so
 * this can be shown; hiding it here would throw that away.
 */
function Skipped({ skipped }: { skipped: NonNullable<InsightReport['skipped']> }) {
  return (
    <div className="card surface-rail ins-skipped" style={vars({ '--rail': 'var(--text-faint)' })}>
      <div className="card-body">
        <h2 className="ins-panel-title">
          Not run
          <span className="count-badge">{fmtCount(skipped.length)}</span>
        </h2>
        <p className="hint">
          These insights did not have what they needed, so they found nothing — which is not the
          same as there being nothing to find.
        </p>
        <ul className="ins-skipped-list">
          {skipped.map((s, i) => (
            <li key={`${s.insight}:${i}`}>
              <code>{s.insight}</code>
              <span>{s.reason}</span>
            </li>
          ))}
        </ul>
      </div>
    </div>
  );
}

/**
 * The empty report.
 *
 * The most consequential copy on the page, for the same reason the Exposure
 * page's zero-result panel is: an operator who reads "no findings" as "nothing
 * wrong" has been actively misled by a tool that only ever looked at a list of
 * what exists. The reasons are named specifically rather than hedged, because a
 * vague caveat gets skipped and a specific one tells the reader what to check.
 */
function NoFindings() {
  return (
    <div className="card surface-rail ins-empty" style={vars({ '--rail': 'var(--warn)' })}>
      <div className="card-body">
        <div className="ins-empty-head">
          <Icon name="alert" size={20} />
          <h2>No findings — which is not a clean bill of health</h2>
        </div>

        <p>
          Every insight is derived from assets this audit happened to collect. Nothing here searched
          for problems in your estate; it looked for patterns in a list, and the list is the limit.
        </p>

        <ul className="ins-reasons">
          <li>
            <strong>An inventory cannot see consumption, traffic, or intent.</strong> Whole classes
            of problem — an over-provisioned instance, a rule nobody has matched in a year, a bucket
            that is public on purpose — are invisible to it by construction, not by omission.
          </li>
          <li>
            <strong>No raw payloads.</strong> The stream only carries <code>raw</code> when the
            server was started with <code>--include-raw</code>. Every insight that parses a spec is
            skipped without it, and the report says which above.
          </li>
          <li>
            <strong>A thin graph.</strong> Insights that read the topology inherit its gaps: a
            cross-provider join needs an address the inventory actually collected, and a policy view
            needs a policy document to have been read.
          </li>
          <li>
            <strong>Only what the credentials could see.</strong> A skipped provider, a compartment
            outside the scope, or a token missing one read permission removes assets — and every
            finding that would have been about them.
          </li>
        </ul>

        <p className="ins-empty-foot">
          A finding here is evidence. The absence of one is the absence of evidence.
        </p>
      </div>
    </div>
  );
}
