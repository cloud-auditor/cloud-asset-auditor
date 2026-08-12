'use client';

import { useEffect, useRef, useState } from 'react';
import { Icon, type UIIconName } from '@/lib/icons';

export interface StatTileProps {
  label: string;
  /** Numbers count up to their new value; strings are shown verbatim. */
  value: number | string;
  sub?: React.ReactNode;
  icon?: UIIconName;
  /** A <Sparkline>, parked bottom-right by `.stat .spark`. */
  spark?: React.ReactNode;
  /** Overrides the value's colour — for a tile that has gone red. */
  color?: string;
  /** Tints the value with the accent while a run streams into it. */
  live?: boolean;
  title?: string;
}

/**
 * Counts the rendered value to a new one over ~400ms.
 *
 * Deliberately a local copy of Nav's `Ticker` rather than an import: this one
 * animates from the value on screen (so an interrupted run picks up where the
 * eye left it), returns the intermediate so the caller can reserve width for
 * it, and has to no-op for non-numeric tiles. Sharing one component would mean
 * one of the two callers carrying props it never uses.
 */
function useCountUp(target: number | null): number | null {
  const [display, setDisplay] = useState(target);
  const shown = useRef(target);

  useEffect(() => {
    if (target === null) {
      shown.current = null;
      setDisplay(null);
      return;
    }
    const from = shown.current;
    if (from === null || from === target) {
      shown.current = target;
      setDisplay(target);
      return;
    }
    // matchMedia is read here rather than at module scope: the export is
    // prerendered, where there is no window.
    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      shown.current = target;
      setDisplay(target);
      return;
    }

    const t0 = performance.now();
    let raf = 0;
    const step = (now: number) => {
      // Clamped at both ends — see the matching note in Nav's Ticker. A rAF
      // timestamp earlier than t0 would otherwise drive the cubic ease
      // negative and paint a nonsense value that sticks, because `shown` is
      // where the next animation starts from.
      const p = Math.min(1, Math.max(0, (now - t0) / 400));
      const eased = 1 - Math.pow(1 - p, 3);
      const v = Math.round(from + (target - from) * eased);
      shown.current = v;
      setDisplay(v);
      if (p < 1) raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [target]);

  return display;
}

export function StatTile({ label, value, sub, icon, spark, color, live, title }: StatTileProps) {
  const numeric = typeof value === 'number';
  const counted = useCountUp(numeric ? value : null);

  const settled = numeric ? value.toLocaleString() : value;
  const text = numeric ? (counted ?? value).toLocaleString() : value;

  // Width is reserved for the longer of "where it is" and "where it is going",
  // so crossing 999 → 1,000 mid-count cannot nudge the tiles beside it. `ch` is
  // the width of a zero, and .stat .v is tabular, so every digit matches it;
  // separators are narrower, which only ever over-reserves.
  const reserve = Math.max(settled.length, text.length);

  return (
    <div className="card stat" title={title}>
      <div className="k">
        {icon && <Icon name={icon} size={13} />}
        {label}
      </div>
      <div className="v" style={color ? { color } : undefined}>
        <span
          className="tick"
          data-live={live ? 'true' : undefined}
          style={{ minWidth: `${reserve}ch` }}
        >
          {text}
        </span>
      </div>
      {sub && <div className="sub truncate">{sub}</div>}
      {spark && <span className="spark">{spark}</span>}
    </div>
  );
}

/** The tile's own shape while a first run is still producing nothing to show —
 *  the row keeps its height, so nothing below it jumps when values land. */
export function StatTileSkeleton() {
  return (
    <div className="card stat" aria-hidden>
      <div className="k">
        <span className="skeleton" style={{ display: 'block', width: 62, height: 9 }} />
      </div>
      <div className="v">
        <span className="skeleton" style={{ display: 'block', width: 74, height: 26 }} />
      </div>
    </div>
  );
}
