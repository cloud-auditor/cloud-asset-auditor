'use client';

import { useId } from 'react';

export interface SparklineProps {
  /** One value per bucket, oldest first. */
  values: readonly number[];
  width?: number;
  height?: number;
  /** Accessible summary. Without it the shape is decorative and hidden. */
  label?: string;
  className?: string;
}

/** Keeps the 2px stroke off the edge of the box, where it would be half-clipped. */
const PAD = 2;

/**
 * A throughput shape, not a chart: no axes, no gridlines, no ticks.
 *
 * It answers one question — "is the stream steady, spiky, or stalling?" — and
 * every number it could label is already on the tile it sits in. Adding a scale
 * to something 26px tall would cost more ink than the signal is worth.
 */
export function Sparkline({ values, width = 68, height = 26, label, className }: SparklineProps) {
  // Before the early return: hooks may not run conditionally.
  const gid = useId();

  if (values.length === 0) return null;

  // A single bucket has no horizontal extent to interpolate across, so it is
  // doubled into a flat line rather than drawn as a lone dot the eye reads as
  // noise. Doubling also keeps the divisor below non-zero.
  const pts = values.length === 1 ? [values[0], values[0]] : values;

  // An all-zero run (every provider stalled) is a real state, and dividing by
  // its max would be a NaN in every coordinate — floor the scale at 1 so it
  // draws as the flat line it actually is.
  const max = Math.max(...pts, 1);
  const top = PAD;
  const bottom = height - PAD;
  const stepX = (width - PAD * 2) / (pts.length - 1);

  const coords = pts.map((v, i): [number, number] => [
    PAD + i * stepX,
    bottom - (v / max) * (bottom - top),
  ]);

  const line = coords
    .map(([x, y], i) => `${i === 0 ? 'M' : 'L'}${x.toFixed(2)} ${y.toFixed(2)}`)
    .join('');
  // The fill drops to the box floor rather than to the line's own minimum, so a
  // run that never reaches zero still reads as area over a baseline.
  const area = `${line}L${(width - PAD).toFixed(2)} ${height}L${PAD} ${height}Z`;

  return (
    <svg
      width={width}
      height={height}
      viewBox={`0 0 ${width} ${height}`}
      className={className}
      role={label ? 'img' : undefined}
      aria-hidden={label ? undefined : true}
      aria-label={label}
      style={{ display: 'block', overflow: 'visible' }}
    >
      {label && <title>{label}</title>}
      <defs>
        {/* Paints go through `style`, not presentation attributes: var() is only
            substituted in CSS declarations, so stroke="var(--accent)" would
            resolve to nothing. */}
        <linearGradient id={gid} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" style={{ stopColor: 'var(--accent)', stopOpacity: 0.34 }} />
          <stop offset="100%" style={{ stopColor: 'var(--accent-2)', stopOpacity: 0 }} />
        </linearGradient>
      </defs>
      <path d={area} fill={`url(#${gid})`} stroke="none" />
      <path
        d={line}
        fill="none"
        strokeWidth={2}
        strokeLinecap="round"
        strokeLinejoin="round"
        style={{ stroke: 'var(--accent)' }}
      />
    </svg>
  );
}
