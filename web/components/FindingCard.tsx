'use client';

import Link from 'next/link';
import { useState } from 'react';
import { providerColor } from '@/lib/colors';
import { vars } from '@/lib/css';
import { fmtCount } from '@/lib/format';
import { AssetIcon, Icon } from '@/lib/icons';
import {
  NO_MONEY,
  SEVERITY_HELP,
  findingCaveat,
  severityColor,
  type Finding,
  type FindingRow,
  type AssetRefLite,
} from '@/lib/insights';

/**
 * How many detail rows are drawn before the reader has to ask for the rest.
 * The Go renderers cap their own tables for the same reason (`WithMaxRows`);
 * a finding over a large estate can carry hundreds of rows, and a card that
 * unrolls into three screens stops being a card.
 */
const ROW_PAGE = 8;

/**
 * One derived finding.
 *
 * The layout is an argument, not a decoration, and its order is the argument's
 * order: what was observed (title, summary, count), what it was derived from
 * (basis), **what it cannot know (caveat)**, and only then the rows.
 *
 * The caveat sits above the table deliberately, and is styled to be impossible
 * to skip. It matches internal/insight's table and markdown renderers, which
 * put the caveat before the detail rows and have a test asserting exactly that
 * (`TestRenderTable_CaveatTravelsWithEveryFinding`). The reasoning is the same
 * on every surface: a reader who has already scanned twelve rows of resource
 * names has formed their conclusion, and a qualification arriving afterwards
 * does not un-form it. It is also why the caveat is not a tooltip, not a
 * disclosure, and not pooled into a page footer — a finding whose caveat the
 * reader never sees is worse than no finding, because it spends the operator's
 * trust on a claim the tool cannot support.
 */
export function FindingCard({ finding }: { finding: Finding }) {
  const [expanded, setExpanded] = useState(false);
  const caveat = findingCaveat(finding);
  const rows = finding.rows ?? [];
  const shown = expanded ? rows : rows.slice(0, ROW_PAGE);
  const sev = severityColor(finding.severity);

  return (
    <article className="card surface-rail finding" style={vars({ '--rail': sev })}>
      <div className="card-body finding-body">
        <header className="finding-head">
          {/* One inline colour drives the chip's text, border and fill via
              currentColor, so a severity is defined in exactly one place. */}
          <span
            className="finding-sev"
            style={{ color: sev }}
            title={SEVERITY_HELP[finding.severity] ?? finding.severity}
          >
            {finding.severity}
          </span>
          <h3 className="finding-title">{finding.title}</h3>
          {finding.count > 0 && (
            <span
              className="count-badge"
              title="The magnitude this finding's summary quotes. It need not equal the number of rows below — a finding may count namespaces while listing pods."
            >
              {fmtCount(finding.count)}
            </span>
          )}
          <span className="spacer" />
          <code className="finding-id" title="Stable id — what a CI gate allowlists and two reports diff on.">
            {finding.id}
          </code>
        </header>

        <p className="finding-summary">{finding.summary}</p>

        {finding.total && (
          <p className="finding-total">
            <span className="finding-total-figure mono">{finding.total.display || NO_MONEY}</span>
            <span className="hint"> per month</span>
          </p>
        )}

        <div className="finding-evidence">
          {finding.basis.trim() !== '' && (
            <p className="finding-basis">
              <span className="finding-label">Derived from</span>
              {finding.basis}
            </p>
          )}

          {/* The whole feature rests on this block being present and read. */}
          <p className={`finding-caveat${caveat.supplied ? '' : ' missing'}`}>
            <span className="finding-label">
              {caveat.supplied ? 'Cannot know' : 'No caveat supplied'}
            </span>
            {caveat.text}
          </p>
        </div>

        {rows.length > 0 && (
          <div className="finding-rows">
            <table>
              <thead>
                <tr>
                  <th scope="col">Subject</th>
                  <th scope="col">Observation</th>
                  <th scope="col" className="num">
                    Value
                  </th>
                </tr>
              </thead>
              <tbody>
                {shown.map((r, i) => (
                  <Row key={`${r.asset?.id ?? r.label}:${i}`} row={r} />
                ))}
              </tbody>
            </table>

            {rows.length > ROW_PAGE && (
              <div className="finding-more">
                <button type="button" className="btn ghost sm" onClick={() => setExpanded(!expanded)}>
                  <Icon name="chevron" size={12} />
                  {expanded ? 'Show fewer' : `Show all ${fmtCount(rows.length)}`}
                </button>
              </div>
            )}

            {/* Only when the two genuinely disagree: a row count that happens to
                match the finding's count needs no explanation, and a note that
                fires every time is a note nobody reads. */}
            {finding.count > 0 && finding.count !== rows.length && (
              <p className="hint finding-rowsnote">
                This finding counts {fmtCount(finding.count)} but lists {fmtCount(rows.length)}. Rows
                are frequently a sample, and a finding may count one thing while listing another —
                the JSON export is where completeness lives.
              </p>
            )}
          </div>
        )}
      </div>
    </article>
  );
}

function Row({ row }: { row: FindingRow }) {
  return (
    <tr>
      <td className="finding-subject">
        <AssetLink asset={row.asset} label={row.label} />
        {row.related && row.related.length > 0 && (
          <span className="finding-related">
            {row.related.map((r, i) => (
              <AssetLink key={`${r.provider}:${r.id}:${i}`} asset={r} label={r.id} small />
            ))}
          </span>
        )}
      </td>
      <td className="finding-fact">{row.fact}</td>
      <td className="num mono">{row.money ? row.money.display : row.value}</td>
    </tr>
  );
}

/**
 * A row's subject, linked into the Assets page with the id pre-searched.
 *
 * Assets rather than Topology — which is where PathChain's pills go — because
 * the questions differ. A node in a reachability chain prompts "what else
 * touches this", a spatial question; a finding's row prompts "what *is* this
 * thing, and does the record justify the claim", which is the drawer's full
 * record. An aggregate row (a namespace, a region) carries no asset at all and
 * renders as plain text rather than as a link to nothing.
 */
function AssetLink({
  asset,
  label,
  small,
}: {
  asset?: AssetRefLite;
  label: string;
  small?: boolean;
}) {
  if (!asset || !asset.id) {
    return <span className={small ? 'finding-ref static' : 'finding-name'}>{label}</span>;
  }
  const title = `${label}\n${asset.type} · ${asset.provider}\n${asset.id}`;
  return (
    <Link
      className={small ? 'finding-ref' : 'finding-name'}
      href={`/assets/?q=${encodeURIComponent(asset.id)}`}
      title={title}
    >
      <span className="dot" style={{ background: providerColor(asset.provider) }} />
      <AssetIcon type={asset.type} size={small ? 11 : 13} />
      <span className="truncate">{label}</span>
    </Link>
  );
}
