'use client';

import { useCallback, useMemo, useRef } from 'react';
import { bounds, type LayoutNode } from '@/lib/layout';
import { providerColor } from '@/lib/colors';

/** Viewport rectangle in graph coordinates. */
export interface MinimapView {
  x: number;
  y: number;
  w: number;
  h: number;
}

export interface MinimapProps {
  nodes: LayoutNode[];
  view: MinimapView;
  /**
   * Bumped by the canvas whenever node positions moved. `nodes` is mutated in
   * place by the force simulation, so its array identity says nothing about
   * whether the picture changed — this is what the memos below key on.
   */
  version: number;
  /** Recentre the main view on a point in graph space. */
  onCenter: (x: number, y: number) => void;
}

const W = 168;
const H = 112;
const PAD = 6;

/** Arrow-key pan, as a fraction of the visible width. */
const NUDGE = 0.18;

/**
 * An overview of the whole graph with the current viewport drawn on it.
 *
 * Strictly a reader of positions: it never ticks the simulation and never
 * touches a node, so dragging it around is free no matter how big the graph
 * is. Everything it can do is a `setView` in the parent.
 */
export function Minimap({ nodes, view, version, onCenter }: MinimapProps) {
  const btnRef = useRef<HTMLButtonElement>(null);
  const dragging = useRef(false);

  // `version` is the real dependency; `nodes` is listed because a new layout
  // swaps the array wholesale and the counter does not reset with it.
  const frame = useMemo(() => {
    const b = bounds(nodes, 40);
    const gw = Math.max(b.maxX - b.minX, 1);
    const gh = Math.max(b.maxY - b.minY, 1);
    const scale = Math.min((W - PAD * 2) / gw, (H - PAD * 2) / gh);
    return {
      scale,
      ox: PAD + (W - PAD * 2 - gw * scale) / 2 - b.minX * scale,
      oy: PAD + (H - PAD * 2 - gh * scale) / 2 - b.minY * scale,
    };
    // `version` is a deliberate extra dependency — it is the only signal that
    // the in-place-mutated `nodes` moved.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes, version]);

  /**
   * One path per provider rather than one element per node: at a few hundred
   * nodes the per-element cost is what would make the minimap the expensive
   * part of a settle frame, and grouping by colour is the only distinction the
   * map has room to make. Squares, not circles — at 2px nobody can tell, and
   * a square is four path commands where a circle is two arcs.
   */
  const dots = useMemo(() => {
    const byProvider = new Map<string, string[]>();
    for (const n of nodes) {
      const x = n.x * frame.scale + frame.ox;
      const y = n.y * frame.scale + frame.oy;
      // Collapsed/high-degree nodes get a slightly bigger mark so the map has
      // the same visual hierarchy as the canvas it summarises.
      const s = n.r > 14 ? 3.6 : 2.2;
      const half = s / 2;
      const list = byProvider.get(n.asset.provider);
      const d = `M${(x - half).toFixed(1)} ${(y - half).toFixed(1)}h${s}v${s}h${-s}z`;
      if (list) list.push(d);
      else byProvider.set(n.asset.provider, [d]);
    }
    return [...byProvider].map(([provider, parts]) => ({ provider, d: parts.join('') }));
  }, [nodes, frame]);

  const toGraph = useCallback(
    (clientX: number, clientY: number) => {
      const rect = btnRef.current?.getBoundingClientRect();
      if (!rect) return null;
      // The svg is laid out at its intrinsic size, so client px map 1:1 onto
      // the minimap's own coordinate system.
      return {
        x: (clientX - rect.left - frame.ox) / frame.scale,
        y: (clientY - rect.top - frame.oy) / frame.scale,
      };
    },
    [frame],
  );

  const moveTo = useCallback(
    (clientX: number, clientY: number) => {
      const p = toGraph(clientX, clientY);
      if (p) onCenter(p.x, p.y);
    },
    [onCenter, toGraph],
  );

  const onKeyDown = (e: React.KeyboardEvent) => {
    const step = view.w * NUDGE;
    const cx = view.x + view.w / 2;
    const cy = view.y + view.h / 2;
    switch (e.key) {
      case 'ArrowLeft':
        onCenter(cx - step, cy);
        break;
      case 'ArrowRight':
        onCenter(cx + step, cy);
        break;
      case 'ArrowUp':
        onCenter(cx, cy - step);
        break;
      case 'ArrowDown':
        onCenter(cx, cy + step);
        break;
      default:
        return;
    }
    e.preventDefault();
  };

  const vp = {
    x: view.x * frame.scale + frame.ox,
    y: view.y * frame.scale + frame.oy,
    w: Math.max(view.w * frame.scale, 3),
    h: Math.max(view.h * frame.scale, 3),
  };

  /**
   * Everything *outside* the viewport, as one even-odd path: the outer
   * rectangle is the whole map, the inner one punches the viewport out of it.
   *
   * Dimming the rest is what makes the viewport legible in the case that
   * matters most — fit-to-view, where the viewport is larger than the graph's
   * extent, so its outline lies outside the map entirely and a stroked
   * rectangle alone draws precisely nothing. Even-odd needs no clamping for
   * that: the parts of the inner rectangle that fall outside the map simply
   * fall outside the SVG.
   */
  const scrim =
    `M0 0H${W}V${H}H0Z` +
    `M${vp.x.toFixed(1)} ${vp.y.toFixed(1)}h${vp.w.toFixed(1)}v${vp.h.toFixed(1)}h${(-vp.w).toFixed(1)}z`;

  return (
    // A <button> rather than a focusable <div>: it is genuinely a control, and
    // native button semantics give keyboard focus, an accessible name, and the
    // Space/Enter activation below for free.
    <button
      ref={btnRef}
      type="button"
      className="topo-minimap"
      aria-label="Minimap. Click or drag to recentre the view; arrow keys pan."
      // No preventDefault on pointerdown: it would suppress the focus the
      // button is owed on click. Touch scrolling is handled by `touch-action`
      // in topology.css instead.
      onPointerDown={(e) => {
        e.currentTarget.setPointerCapture(e.pointerId);
        dragging.current = true;
        moveTo(e.clientX, e.clientY);
      }}
      onPointerMove={(e) => {
        if (dragging.current) moveTo(e.clientX, e.clientY);
      }}
      onPointerUp={() => {
        dragging.current = false;
      }}
      onPointerCancel={() => {
        dragging.current = false;
      }}
      onKeyDown={onKeyDown}
    >
      <svg width={W} height={H} viewBox={`0 0 ${W} ${H}`} aria-hidden="true">
        {dots.map((g) => (
          <path key={g.provider} d={g.d} fill={providerColor(g.provider)} fillOpacity={0.9} />
        ))}
        {/* Both drawn last, so neither is buried under a dense cluster of
            dots — the viewport is the one thing on here that has to be found
            at a glance. */}
        <path className="scrim" d={scrim} fillRule="evenodd" />
        <rect className="vp" x={vp.x} y={vp.y} width={vp.w} height={vp.h} rx={2} />
      </svg>
    </button>
  );
}
