'use client';
import { fmtCount } from '@/lib/format';

export interface BarRow {
  label: string;
  value: number;
  color?: string;
}

export interface BarListProps {
  rows: BarRow[];
  total: number;
  /** Makes rows clickable. Called with the row's label. */
  onSelect?: (label: string) => void;
  selected?: string | null;
  /** Rank numerals are on by default; turn them off for an unordered list. */
  ranked?: boolean;
  emptyText?: string;
}

/**
 * The fill mirrors `.meter-fill` in globals.css — a wash of the row's colour
 * rising to the full value at the tip, so the bar's end is its loudest point.
 */
function fill(color: string): string {
  return `linear-gradient(90deg, color-mix(in srgb, ${color} 45%, transparent), ${color})`;
}

/**
 * A ranked horizontal bar list — the right form for "how much of the
 * inventory is X", where the categories are unbounded (there are hundreds of
 * Kubernetes resource types) and the reader wants rank plus magnitude.
 *
 * Bars are scaled against the largest row, not against `total`: with a long
 * tail, scaling to the total leaves every bar after the first as an invisible
 * sliver. `total` is used only for the percentage label, which is where the
 * share actually belongs.
 */
export function BarList({
  rows,
  total,
  onSelect,
  selected = null,
  ranked = true,
  emptyText = 'No data.',
}: BarListProps) {
  if (rows.length === 0) return <div className="faint">{emptyText}</div>;
  const max = Math.max(...rows.map((r) => r.value), 1);

  return (
    <div
      className="bars"
      style={
        ranked
          ? ({ '--bar-cols': '20px minmax(84px, 190px) 1fr auto' } as React.CSSProperties)
          : undefined
      }
    >
      {rows.map((r, i) => {
        const color = r.color ?? 'var(--accent)';
        const on = selected === r.label;
        const body = (
          <>
            {ranked && <span className="rank">{i + 1}</span>}
            <span className="truncate mono" title={r.label}>
              {r.label}
            </span>
            <span className="bar-track">
              <span
                className="bar-fill"
                style={{ width: `${(r.value / max) * 100}%`, background: fill(color) }}
              />
            </span>
            <span className="n">
              {fmtCount(r.value)}
              {total > 0 && <span className="faint"> {((r.value / total) * 100).toFixed(0)}%</span>}
            </span>
          </>
        );

        if (!onSelect) {
          return (
            <div className="bar-row" key={r.label}>
              {body}
            </div>
          );
        }
        return (
          <button
            type="button"
            className="bar-row"
            key={r.label}
            data-on={on ? 'true' : undefined}
            aria-pressed={on}
            onClick={() => onSelect(r.label)}
          >
            {body}
          </button>
        );
      })}
    </div>
  );
}
