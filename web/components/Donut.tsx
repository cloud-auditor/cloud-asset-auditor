'use client';

import { useEffect, useMemo, useState } from 'react';
import { fmtCount } from '@/lib/format';

export interface DonutSegment {
  key: string;
  label: string;
  value: number;
  /** Any CSS colour — in practice `providerColor(name)`. */
  color: string;
}

export interface DonutProps {
  segments: readonly DonutSegment[];
  /** Rendered diameter in px. */
  size?: number;
  /** Ring thickness in px. */
  thickness?: number;
  /** The cross-filtered key, if the consumer is filtering on one. */
  selected?: string | null;
  /** Called with a key to filter, or null to clear. Omit to make the ring inert. */
  onSelect?: (key: string | null) => void;
  /** Noun under the total in the centre. */
  unit?: string;
  legend?: boolean;
  className?: string;
}

/** Part-to-whole reads at a glance only while the slice count stays small; past
 *  this the ring is a legend with decoration attached, and the ranked bar lists
 *  below it already do that job better. */
const MAX_SEGMENTS = 6;

/** Under this share a slice is thinner than the gaps around it. */
const MIN_SHARE = 0.02;

const OTHER = '__other__';

interface Slice extends DonutSegment {
  share: number;
  /** Distance along the circumference where the arc starts. */
  offset: number;
  /** Arc length, gap already deducted. */
  len: number;
}

function pct(share: number): string {
  // Anything non-zero that rounds to 0% is reported as <1% instead: "0%" next to
  // a visible slice reads as a rendering bug.
  const p = share * 100;
  if (p > 0 && p < 0.5) return '<1%';
  return `${p.toFixed(p < 10 ? 1 : 0)}%`;
}

/**
 * A composition ring.
 *
 * Drawn as stroked arcs on one circle via stroke-dasharray/dashoffset rather
 * than as <path> wedges: there is no trigonometry to get wrong, no large-arc
 * flag to flip at 180°, and the whole ring animates by transitioning a single
 * number per slice.
 */
export function Donut({
  segments,
  size = 148,
  thickness = 16,
  selected = null,
  onSelect,
  unit = 'assets',
  legend = true,
  className,
}: DonutProps) {
  const [hovered, setHovered] = useState<string | null>(null);
  const [drawn, setDrawn] = useState(false);

  // Two frames, not one: a transition needs a painted "from" value, and a
  // single rAF can still be flushed in the same frame as the initial paint.
  // Reduced motion is handled globally — the transition collapses to 0.001ms
  // and this just becomes a state flip.
  useEffect(() => {
    let inner = 0;
    const outer = requestAnimationFrame(() => {
      inner = requestAnimationFrame(() => setDrawn(true));
    });
    return () => {
      cancelAnimationFrame(outer);
      cancelAnimationFrame(inner);
    };
  }, []);

  const geom = useMemo(() => {
    // The viewBox is a fixed 100 units so the maths is scale-free; stroke width
    // and gap are converted from px so they render at the size asked for
    // regardless of the diameter.
    const sw = (thickness * 100) / size;
    const gap = (2 * 100) / size;
    const r = 50 - sw / 2 - 1;
    return { sw, gap, r, circumference: 2 * Math.PI * r };
  }, [size, thickness]);

  const model = useMemo(() => {
    const positive = segments.filter((s) => s.value > 0).sort((a, b) => b.value - a.value);
    const total = positive.reduce((n, s) => n + s.value, 0);
    if (total === 0) return { slices: [] as Slice[], total: 0, otherCount: 0 };

    const keep: DonutSegment[] = [];
    const rest: DonutSegment[] = [];
    for (const s of positive) {
      if (keep.length < MAX_SEGMENTS && s.value / total >= MIN_SHARE) keep.push(s);
      else rest.push(s);
    }
    // One demoted slice is not a crowd. Folding a single named provider into an
    // anonymous "Other" loses its identity and gains nothing, so take it back.
    if (rest.length === 1 && keep.length < MAX_SEGMENTS) {
      const only = rest.pop();
      if (only) keep.push(only);
    }

    const drawable: DonutSegment[] = keep.slice();
    if (rest.length > 0) {
      drawable.push({
        key: OTHER,
        label: 'Other',
        value: rest.reduce((n, s) => n + s.value, 0),
        color: 'var(--text-faint)',
      });
    }

    const { circumference, gap } = geom;
    let cum = 0;
    const slices: Slice[] = drawable.map((s) => {
      const share = s.value / total;
      // Half the gap at each end keeps every slice's spacing even, including
      // the seam where the last one meets the first.
      const offset = cum * circumference + gap / 2;
      const len = Math.max(0, share * circumference - gap);
      cum += share;
      return { ...s, share, offset, len };
    });

    return { slices, total, otherCount: rest.length };
  }, [segments, geom]);

  const { slices, total, otherCount } = model;
  const active = hovered ?? selected;
  const activeSlice = active === null ? null : (slices.find((s) => s.key === active) ?? null);

  if (total === 0) return null;

  const summary =
    `${fmtCount(total)} ${unit} across ${slices.length} ` +
    `${slices.length === 1 ? 'segment' : 'segments'}: ` +
    slices.map((s) => `${s.label} ${fmtCount(s.value)} (${pct(s.share)})`).join(', ');

  const pick = (key: string) => onSelect?.(selected === key ? null : key);

  return (
    <div className={`donut-wrap${className ? ` ${className}` : ''}`}>
      <div className="donut" style={{ width: size, height: size }}>
        <svg width={size} height={size} viewBox="0 0 100 100" role="img" aria-label={summary}>
          {/* The track carries the ring's shape while the arcs are still
              drawing in, so the card does not start life as an empty hole. */}
          <circle
            cx="50"
            cy="50"
            r={geom.r}
            fill="none"
            strokeWidth={geom.sw}
            style={{ stroke: 'var(--surface-2)' }}
          />
          {/* Slices grow together rather than in a staggered sweep: the delay
              would have to sit on the element, where it also delays the
              hover-dim, and a per-slice ladder pushes the ring's total motion
              past the 300ms budget. */}
          <g transform="rotate(-90 50 50)">
            {slices.map((s) => (
              <circle
                key={s.key}
                className="donut-seg"
                cx="50"
                cy="50"
                r={geom.r}
                fill="none"
                strokeWidth={geom.sw}
                data-dim={active !== null && active !== s.key ? 'true' : undefined}
                data-pick={onSelect && s.key !== OTHER ? 'true' : undefined}
                style={{
                  stroke: s.color,
                  strokeDasharray: drawn
                    ? `${s.len} ${geom.circumference - s.len}`
                    : `0 ${geom.circumference}`,
                  strokeDashoffset: -s.offset,
                }}
                onMouseEnter={() => setHovered(s.key)}
                onMouseLeave={() => setHovered(null)}
                onClick={s.key === OTHER ? undefined : () => pick(s.key)}
              />
            ))}
          </g>
        </svg>

        {/* The readout is HTML over the SVG rather than <text>: it inherits the
            type tokens, truncates, and takes tabular numerals for free. */}
        <div className="donut-center" aria-hidden>
          {activeSlice ? (
            <>
              <span className="donut-c-v tick">{fmtCount(activeSlice.value)}</span>
              <span className="donut-c-k truncate" title={activeSlice.label}>
                {activeSlice.label}
              </span>
              <span className="donut-c-s">{pct(activeSlice.share)}</span>
            </>
          ) : (
            <>
              <span className="donut-c-v tick">{fmtCount(total)}</span>
              <span className="donut-c-k">{unit}</span>
            </>
          )}
        </div>
      </div>

      {legend && (
        <ul className="donut-legend">
          {slices.map((s) => {
            const row = (
              <>
                <span className="donut-swatch" style={{ background: s.color }} />
                <span className="truncate" title={s.label}>
                  {s.label}
                </span>
                <span className="mono donut-n">{fmtCount(s.value)}</span>
                <span className="mono faint donut-p">{pct(s.share)}</span>
              </>
            );

            // "Other" is a bucket, not an entity, so there is nothing to filter
            // to — it renders inert. Its numbers are on the row itself, so a
            // keyboard user loses only the hover highlight, not information.
            if (s.key === OTHER) {
              return (
                <li
                  key={s.key}
                  className="donut-legend-row"
                  data-dim={active !== null && active !== s.key ? 'true' : undefined}
                  onMouseEnter={() => setHovered(s.key)}
                  onMouseLeave={() => setHovered(null)}
                >
                  {row}
                  <span className="hint donut-other">
                    {otherCount} under {MIN_SHARE * 100}%
                  </span>
                </li>
              );
            }

            return (
              <li key={s.key}>
                <button
                  type="button"
                  className="donut-legend-row"
                  data-dim={active !== null && active !== s.key ? 'true' : undefined}
                  data-on={selected === s.key ? 'true' : undefined}
                  aria-pressed={onSelect ? selected === s.key : undefined}
                  disabled={!onSelect}
                  onMouseEnter={() => setHovered(s.key)}
                  onMouseLeave={() => setHovered(null)}
                  onFocus={() => setHovered(s.key)}
                  onBlur={() => setHovered(null)}
                  onClick={() => pick(s.key)}
                >
                  {row}
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}
