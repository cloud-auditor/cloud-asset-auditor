'use client';

import { Fragment, useCallback, useEffect, useId, useLayoutEffect, useMemo, useRef, useState } from 'react';
import { Minimap } from './Minimap';
import {
  SETTLE_TICKS,
  alphaAt,
  bounds,
  bowOffsets,
  buildLayout,
  edgeGeometry,
  reseed,
  tick,
  type LayoutEdge,
  type LayoutNode,
} from '@/lib/layout';
import { groupHull, roundedPath } from '@/lib/hull';
import {
  EDGE_LABELS,
  EDGE_SOURCES,
  edgeColor,
  providerColor,
  providerFill,
  statusTone,
  toneColor,
} from '@/lib/colors';
import { AssetIcon, Icon, iconKeyForType, type IconKey } from '@/lib/icons';
import { match } from '@/lib/fuzzy';
import {
  COLLAPSED_NODE_TYPE,
  EDGE_KINDS,
  type Asset,
  type DetailLevel,
  type Edge,
  type GroupBy,
} from '@/lib/types';

interface Props {
  nodes: Asset[];
  edges: Edge[];
  /** Edge kinds to draw. Empty means "all". */
  kinds: Set<string>;
  onToggleKind: (kind: string) => void;
  onResetKinds: () => void;
  groupBy: GroupBy;
  detail: DetailLevel;
  /** An asset id from `?focus=`, selected and centred once the layout settles. */
  focusId?: string | null;
}

/** The simulation's own coordinate space. The viewBox decides what of it is
 *  on screen, so these are not pixels and never need to match the container. */
const WIDTH = 1200;
const HEIGHT = 760;

/** Ticks per frame: the sim converges in wall-clock time the user is willing
 *  to watch, without a slow-motion first second. The tick *count* lives in
 *  lib/layout.ts (SETTLE_TICKS) with the cooling schedule it belongs to. */
const TICKS_PER_FRAME = 3;

/** Past this many nodes a React repaint costs more than the physics does, so
 *  the settle animation is sampled every few frames instead of every one. The
 *  simulation itself still runs at full rate — only the picture is sampled. */
const BIG_GRAPH = 260;
const BIG_GRAPH_RENDER_EVERY = 4;

/** Screen px of node radius below which a 15-line glyph is a smudge. Hiding it
 *  is more honest than drawing it. */
const GLYPH_MIN_PX = 8;

/** Arrowhead length, in graph units. Also the clearance the curve leaves at
 *  the target node so the head sits on the boundary rather than under it. */
const ARROW = 9;

/** Flow dots are lovely at 200 edges and noise at 2,000 — and they are a
 *  second path element each. Above this, traffic-allow edges just get colour. */
const MAX_FLOW_DECORATED = 600;

/** A hull needs three members to say anything: around two nodes it is a
 *  lozenge that draws a region where there is only a line between them, and
 *  around one it is a circle already drawn by the node itself. */
const MIN_HULL_MEMBERS = 3;

const HULL_PAD = 34;
const HULL_ROUND = 30;

/** Zoom limits, as viewBox widths. */
const MIN_VIEW_W = 90;
const MAX_VIEW_W = 24000;
/** How far in "centre on this node" zooms when the view is wider than this. */
const FOCUS_VIEW_W = 900;

interface View {
  x: number;
  y: number;
  w: number;
  h: number;
}

/** One group blob: the ring to draw, and where its label wants to sit. */
interface Hull {
  key: string;
  d: string;
  provider: string;
  label: string;
  /** The ring's topmost vertex, and the outward unit vector there. */
  topX: number;
  topY: number;
  ux: number;
  uy: number;
  /** The ring provably contains a node that is not a member of the group. */
  impure: boolean;
}

export function GraphCanvas({
  nodes,
  edges,
  kinds,
  onToggleKind,
  onResetKinds,
  groupBy,
  detail,
  focusId,
}: Props) {
  const uid = useId().replace(/:/g, '');
  const legendId = `${uid}-legend`;
  const wrapRef = useRef<HTMLDivElement>(null);
  const svgRef = useRef<SVGSVGElement>(null);

  const [frame, setFrame] = useState(0);
  const [settled, setSettled] = useState(false);
  const [runId, setRunId] = useState(0);
  const [selected, setSelected] = useState<string | null>(null);
  const [view, setView] = useState<View>({ x: 0, y: 0, w: WIDTH, h: HEIGHT });
  const [size, setSize] = useState({ w: WIDTH, h: HEIGHT });
  // The search box's text lives here rather than inside NodeSearch because two
  // things read it: the results dropdown, and the label pass — a node you have
  // just searched for has to be labelled whether or not you pick it.
  const [query, setQuery] = useState('');
  const [legendOpen, setLegendOpen] = useState(true);

  // Once the operator has framed the view themselves, the auto-fit that tracks
  // the settling graph must stop fighting them.
  const userFramed = useRef(false);

  /**
   * The dimension the force layout clusters by — not always the one the user
   * picked.
   *
   * At `detail: high` the server has already collapsed each group into a
   * single node, so every cluster would have exactly one member: the group
   * forces degenerate into placing nodes on a ring by group *name*, which
   * overrides the edges with an arrangement that carries no information. At
   * that level the edges are the entire content of the diagram, so the layout
   * is left flat. Below it, clustering is worth having: at `low` it is what
   * makes the hulls honest, and at `medium` it puts a provider's type-nodes
   * together, which is the shape the reader is looking for even without a
   * blob drawn round it.
   */
  const clusterBy = detail === 'high' ? '' : groupBy;

  /**
   * The layout is built from *every* edge, not the visible subset.
   *
   * Toggling an edge-kind chip is a statement about what to draw, not about
   * what the graph is — rebuilding the layout for it would reseed every node
   * and re-run the whole settle, so the diagram would explode and reassemble
   * each time someone glanced at a legend entry. `clusterBy` *is* structure,
   * so it belongs here; in practice the page has already refetched the
   * topology by then, which hands us a new `nodes` array anyway.
   */
  const layout = useMemo(
    () => buildLayout(nodes, edges, WIDTH, HEIGHT, clusterBy),
    [nodes, edges, clusterBy],
  );

  /** Whether group blobs are drawn — see the `hulls` memo, which must agree. */
  const hulled = detail === 'low' && layout.groups.length > 0;

  const fitOf = useCallback((): View => {
    const b = bounds(layout.nodes);
    const aspect = size.w / size.h;
    const first = frameToAspect(b, aspect);
    /*
     * `bounds` knows only about node circles, but this component draws three
     * things outside them: the hull ring, which reaches HULL_PAD past its
     * outermost member; the group label, which sits outside *that*; and node
     * labels, which hang half their width either side. Fitting to the circles
     * alone crops all three — it is what sliced the top group label off the
     * canvas the moment those labels moved outside their blobs.
     *
     * Label sizes are in screen pixels, so their extent in graph units depends
     * on the scale this calculation is producing. The first pass supplies that
     * estimate; a second pass is enough, because the correction is a few
     * percent of the width and re-running it would move the answer by a few
     * percent of that.
     */
    const u = first.w / size.w; // graph units per screen pixel
    const ring = hulled ? HULL_PAD : 0;
    // Half of a typical name, not of the longest possible one: reserving for
    // the widest label that could exist would zoom every graph out by a tenth
    // to protect an edge case that panning already answers.
    const side = ring + LABEL_PX * 10 * CHAR_W * u;
    const top = ring + (hulled ? GROUP_LABEL_PX * 3.2 * u : 0);
    return frameToAspect(
      { minX: b.minX - side, maxX: b.maxX + side, minY: b.minY - top, maxY: b.maxY + ring },
      aspect,
    );
  }, [layout, hulled, size.w, size.h]);

  // The simulation loop must not restart when the window resizes, but it does
  // need the current aspect ratio to auto-frame with. A ref is how it reads
  // "latest" without taking a dependency on it.
  const fitRef = useRef(fitOf);
  fitRef.current = fitOf;

  useLayoutEffect(() => {
    const el = wrapRef.current;
    if (!el) return;
    const measure = () => {
      const r = el.getBoundingClientRect();
      if (r.width > 0 && r.height > 0) setSize({ w: r.width, h: r.height });
    };
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(el);
    return () => ro.disconnect();
  }, []);

  // Keep the viewBox in the container's aspect ratio. With the two matched,
  // preserveAspectRatio never letterboxes, so screen↔graph is a single scale
  // factor and every pointer calculation below stays a one-liner.
  useEffect(() => {
    const aspect = size.w / size.h;
    setView((v) => {
      const h = v.w / aspect;
      return Math.abs(h - v.h) < 0.5 ? v : { ...v, y: v.y + (v.h - h) / 2, h };
    });
  }, [size.w, size.h]);

  const scale = size.w / view.w;

  // Finish the simulation in a rAF loop, re-rendering as it goes so the graph
  // visibly settles rather than popping into place. buildLayout has already
  // run the front of the schedule synchronously, so this picks up at
  // `warmTicks` — what is left here is the short, calm tail.
  useEffect(() => {
    // A rebuilt graph is a different picture, not a moved one. Clustering
    // changes the extent wholesale — the demo inventory spans 656×661 units
    // flat and 828×972 grouped, around different centres — so a viewport the
    // operator framed on the old arrangement leaves the new one half off the
    // canvas. Their framing survives panning, zooming and dragging; it does
    // not survive the graph being rebuilt underneath it.
    userFramed.current = false;
    setSettled(false);
    let i = layout.warmTicks;
    let raf = 0;
    const renderEvery = layout.nodes.length > BIG_GRAPH ? BIG_GRAPH_RENDER_EVERY : 1;
    let sinceRender = 0;

    // Reduced motion means "do not animate the settle", not "do not settle":
    // the ticks still run, chunked across frames so the tab stays responsive,
    // and only the finished picture is painted. Doing it by enlarging the
    // synchronous warm-up instead would trade an animation this user did not
    // want for a frozen tab nobody wants.
    const reduced =
      typeof window.matchMedia === 'function' &&
      window.matchMedia('(prefers-reduced-motion: reduce)').matches;

    const step = () => {
      for (let t = 0; t < TICKS_PER_FRAME && i < SETTLE_TICKS; t++, i++) {
        tick(layout, WIDTH, HEIGHT, alphaAt(i));
      }
      const done = i >= SETTLE_TICKS;
      sinceRender++;
      if (done || (!reduced && sinceRender >= renderEvery)) {
        sinceRender = 0;
        setFrame((n) => n + 1);
        // The graph grows outward as repulsion does its work; tracking it with
        // the viewport keeps it framed the whole way instead of showing a
        // hairball that slowly walks off the canvas.
        if (!userFramed.current) setView(fitRef.current());
      }
      if (done) setSettled(true);
      else raf = requestAnimationFrame(step);
    };

    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
    // Structure only. `runId` is the explicit "start over" signal; everything
    // else the loop needs it reads through fitRef.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [layout, runId]);

  // --- viewport ------------------------------------------------------------

  const toGraph = useCallback(
    (clientX: number, clientY: number) => {
      const rect = svgRef.current?.getBoundingClientRect();
      if (!rect) return { x: 0, y: 0 };
      return {
        x: view.x + ((clientX - rect.left) / rect.width) * view.w,
        y: view.y + ((clientY - rect.top) / rect.height) * view.h,
      };
    },
    [view],
  );

  const zoomBy = useCallback((factor: number, about?: { x: number; y: number }) => {
    userFramed.current = true;
    setView((v) => {
      const w = clamp(v.w * factor, MIN_VIEW_W, MAX_VIEW_W);
      const f = w / v.w; // the factor that actually survived the clamp
      const p = about ?? { x: v.x + v.w / 2, y: v.y + v.h / 2 };
      return { x: p.x - (p.x - v.x) * f, y: p.y - (p.y - v.y) * f, w, h: v.h * f };
    });
  }, []);

  const centerOn = useCallback((x: number, y: number, tighten = false) => {
    userFramed.current = true;
    setView((v) => {
      const w = tighten ? Math.min(v.w, FOCUS_VIEW_W) : v.w;
      const h = v.h * (w / v.w);
      return { x: x - w / 2, y: y - h / 2, w, h };
    });
  }, []);

  const fit = useCallback(() => {
    userFramed.current = true;
    setView(fitOf());
  }, [fitOf]);

  /** Back to the seeded positions and a fresh settle — the one control that
   *  undoes dragging as well as panning. */
  const reset = useCallback(() => {
    setSelected(null);
    userFramed.current = false;
    reseed(layout, WIDTH, HEIGHT);
    setRunId((n) => n + 1);
  }, [layout]);

  // --- pan / drag ----------------------------------------------------------

  const drag = useRef<{ node: LayoutNode | null; px: number; py: number; moved: boolean } | null>(
    null,
  );

  const onPointerDown = (e: React.PointerEvent, node?: LayoutNode) => {
    (e.target as Element).setPointerCapture?.(e.pointerId);
    const p = toGraph(e.clientX, e.clientY);
    if (node) {
      node.fixed = true; // hold it wherever the pointer is while the sim runs
      setSelected(node.key);
    }
    drag.current = { node: node ?? null, px: p.x, py: p.y, moved: false };
  };

  const onPointerMove = (e: React.PointerEvent) => {
    const d = drag.current;
    if (!d) return;
    const p = toGraph(e.clientX, e.clientY);
    d.moved = d.moved || Math.hypot(p.x - d.px, p.y - d.py) > 2;
    if (d.node) {
      d.node.x = p.x;
      d.node.y = p.y;
      setFrame((n) => n + 1);
    } else {
      // Panning moves the viewport opposite the pointer, so the graph tracks
      // the finger rather than sliding away from it.
      userFramed.current = true;
      setView((v) => ({ ...v, x: v.x - (p.x - d.px), y: v.y - (p.y - d.py) }));
    }
  };

  const onPointerUp = () => {
    const d = drag.current;
    drag.current = null;
    if (!d) return;
    if (d.node) {
      d.node.fixed = false;
      return;
    }
    // A press on empty canvas that never turned into a pan is a click on
    // nothing, which is the ordinary way to mean "deselect".
    if (!d.moved) setSelected(null);
  };

  const onWheel = (e: React.WheelEvent) => {
    zoomBy(e.deltaY > 0 ? 1.12 : 1 / 1.12, toGraph(e.clientX, e.clientY));
  };

  // --- selection -----------------------------------------------------------

  const sel = selected ? (layout.byKey.get(selected) ?? null) : null;

  const selEdges = useMemo(
    () =>
      selected ? layout.edges.filter((e) => e.from.key === selected || e.to.key === selected) : [],
    [layout, selected],
  );

  /** The selected node plus everything one edge away — what stays lit while
   *  the rest dims. The selection seeds it explicitly: a node with no edges
   *  appears in none of them, and would otherwise dim itself the moment it
   *  was picked. */
  const neighbours = useMemo(() => {
    const s = new Set<string>();
    if (selected) s.add(selected);
    for (const e of selEdges) {
      s.add(e.from.key);
      s.add(e.to.key);
    }
    return s;
  }, [selEdges, selected]);

  const select = useCallback(
    (n: LayoutNode) => {
      setSelected(n.key);
      centerOn(n.x, n.y, true);
    },
    [centerOn],
  );

  /**
   * Every node matching the search box, best first.
   *
   * Computed once for both consumers. The dropdown shows the head of it; the
   * label pass pins the head of it too (see MAX_PINNED_MATCHES) — searching is
   * how you ask "where is this thing", and answering with a highlighted circle
   * whose name is culled would be a poor answer.
   */
  const matches = useMemo(() => {
    const needle = query.trim();
    if (!needle) return [];
    const scored: { n: LayoutNode; score: number }[] = [];
    for (const n of layout.nodes) {
      const byName = match(needle, n.asset.name || '');
      const byId = match(needle, n.asset.id);
      const best = Math.max(byName?.score ?? -Infinity, byId?.score ?? -Infinity);
      if (best > -Infinity) scored.push({ n, score: best });
    }
    scored.sort((a, b) => b.score - a.score);
    return scored.map((s) => s.n);
  }, [query, layout]);

  /** Nodes whose labels are drawn regardless of the budget, in the order they
   *  get to claim space: the selection, then its neighbourhood, then the
   *  search hits. */
  const pinned = useMemo(() => {
    const seen = new Set<string>();
    const out: string[] = [];
    const add = (k: string) => {
      if (!seen.has(k)) {
        seen.add(k);
        out.push(k);
      }
    };
    if (selected) add(selected);
    for (const k of neighbours) add(k);
    for (const n of matches.slice(0, MAX_PINNED_MATCHES)) add(n.key);
    return out;
  }, [selected, neighbours, matches]);

  // A deep link is a request to look at one node, so it waits for the layout
  // to stop moving — centring on a node mid-settle aims at where it *was*.
  const focusDone = useRef<string | null>(null);
  useEffect(() => {
    if (!settled || !focusId || focusDone.current === focusId) return;
    const hit =
      layout.byKey.get(focusId) ?? layout.nodes.find((n) => n.asset.id === focusId) ?? null;
    focusDone.current = focusId;
    if (hit) select(hit);
  }, [settled, focusId, layout, select]);

  // --- what gets drawn -----------------------------------------------------

  const drawEdges = useMemo(() => {
    const bows = bowOffsets(layout.edges);
    const out: { le: LayoutEdge; bow: number; i: number }[] = [];
    for (let i = 0; i < layout.edges.length; i++) {
      const le = layout.edges[i];
      if (kinds.size === 0 || kinds.has(le.edge.kind)) out.push({ le, bow: bows[i], i });
    }
    return out;
  }, [layout, kinds]);

  const kindCounts = useMemo(() => {
    const m = new Map<string, number>();
    for (const { edge } of layout.edges) m.set(edge.kind, (m.get(edge.kind) ?? 0) + 1);
    return [...m].sort((a, b) => a[0].localeCompare(b[0]));
  }, [layout]);

  /** One marker per colour: an SVG marker cannot inherit the referencing
   *  path's stroke in a way Safari honours, so the arrowhead colour has to be
   *  baked in. Built from the kinds actually present rather than a fixed list,
   *  which is what used to leave gateway/waf/service arrows the wrong colour. */
  const arrows = useMemo(() => {
    const seen = new Map<string, string>();
    for (const [kind] of kindCounts) {
      const c = edgeColor(kind);
      if (!seen.has(c)) seen.set(c, `${uid}-arrow-${seen.size}`);
    }
    return seen;
  }, [kindCounts, uid]);

  /** Glyphs are defined once and stamped with <use>. Per-node <AssetIcon>
   *  elements would be 3–4 extra SVG nodes each to reconcile on every settle
   *  frame, which is the difference between a smooth 300-node graph and a
   *  stuttering one. */
  const glyphKeys = useMemo(() => {
    const s = new Set<IconKey>();
    for (const n of layout.nodes) s.add(iconKeyForType(n.asset.type));
    return [...s];
  }, [layout]);

  const hulls = useMemo(() => {
    // Only at `detail: low`. medium/high already collapsed each group into one
    // node, so a blob around it would be a ring drawn around a single circle.
    // `layout.groups` rather than re-deriving the buckets: the blob has to be
    // drawn around exactly the set the cohesion force pulled together, or it
    // is a ring around a cluster that was never made.
    if (!hulled) return [];
    const out: Hull[] = [];
    for (const g of layout.groups) {
      if (g.members.length < MIN_HULL_MEMBERS) continue;
      // Rebuilt from scratch every settle frame, which sounds expensive until
      // you price it against the frame that produced these positions: the
      // O(n²) force pass at 900 nodes is ~1.2M pair interactions, where this
      // is ~9k points to sort. The hull is noise in that budget.
      const { points, enclosed } = groupHull(g.members, layout.nodes, HULL_PAD);
      const d = roundedPath(points, HULL_ROUND);
      if (!d) continue;

      // The ring's topmost vertex, and the outward direction there — where the
      // group label goes. Anchoring on the bounding box's top *edge* instead
      // (what this used to do) puts the text inside the blob whenever the peak
      // is off-centre, on top of the nodes crowded against it.
      let top = points[0];
      let sx = 0;
      let sy = 0;
      for (const p of points) {
        sx += p.x;
        sy += p.y;
        if (p.y < top.y) top = p;
      }
      const dx = top.x - sx / points.length;
      const dy = top.y - sy / points.length;
      const len = Math.hypot(dx, dy) || 1;

      out.push({
        key: g.key,
        d,
        // Under group-by account/region a group can span providers; the modal
        // one is the honest tint, and it agrees with group-by provider.
        provider: dominantProvider(g.members),
        label: `${elideMiddle(g.key, GROUP_KEY_MAX)} · ${g.members.length}`,
        topX: top.x,
        topY: top.y,
        ux: dx / len,
        uy: dy / len,
        // A convex ring cannot exclude a non-member that sits inside its
        // members' convex closure — at any padding, see lib/hull.ts. When one
        // does, the blob keeps its outline but loses its fill: an outline
        // shows where the cluster is, where shading the ground under a foreign
        // node claims to own it.
        impure: enclosed > 0,
      });
    }
    return out;
    // `frame` is the extra dependency that matters: the hull is a function of
    // node positions, and those are mutated in place by the simulation.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [layout, hulled, frame]);

  /**
   * Which labels this frame can carry. See {@link placeLabels} for the rule.
   *
   * Recomputed on `view` as well as on `frame`, because labels are sized in
   * screen pixels: zooming changes their footprint in graph space and
   * therefore changes what fits, which is the whole mechanism by which zooming
   * in reveals more of them.
   */
  const labels = useMemo(
    () =>
      placeLabels(
        layout.nodes,
        layout.byKey,
        hulls,
        pinned,
        view,
        scale,
        labelBudget(size.w, size.h, scale),
      ),
    // `frame` again: node positions are mutated in place, so it is the only
    // signal that the boxes moved.
    // eslint-disable-next-line react-hooks/exhaustive-deps
    [layout, hulls, pinned, view, scale, size.w, size.h, frame],
  );

  const flowDots = drawEdges.length <= MAX_FLOW_DECORATED;

  return (
    <div className="card graph-wrap topo-graph" ref={wrapRef}>
      <svg
        ref={svgRef}
        viewBox={`${view.x} ${view.y} ${view.w} ${view.h}`}
        onPointerDown={(e) => onPointerDown(e)}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerLeave={onPointerUp}
        onWheel={onWheel}
        role="img"
        // The canvas itself is not keyboard-navigable by design: a few hundred
        // focusable circles would bury the rest of the page in tab stops. The
        // search box reaches any node by name and the inspector's neighbour
        // list walks the graph from there, so nothing here is pointer-only.
        aria-label={`Network topology: ${layout.nodes.length} nodes, ${drawEdges.length} edges drawn.`}
      >
        <defs>
          {[...arrows].map(([color, id]) => (
            <marker
              key={id}
              id={id}
              viewBox="0 0 10 10"
              refX="10"
              refY="5"
              // userSpaceOnUse, so a thick collapsed edge and a hairline one
              // get the same size head — stroke-scaled markers make a ×40 edge
              // look like it is firing a cannonball.
              markerUnits="userSpaceOnUse"
              markerWidth={ARROW}
              markerHeight={ARROW}
              orient="auto"
            >
              <path d="M0 1.4L9.4 5L0 8.6z" fill={color} />
            </marker>
          ))}
          {glyphKeys.map((k) => (
            <g key={k} id={`${uid}-g-${k}`}>
              <AssetIcon iconKey={k} size={24} strokeWidth={1.9} />
            </g>
          ))}
        </defs>

        {/* Hull outlines only — their labels are drawn last, over everything,
            because they are the layer that survives losing all the others. */}
        <g className="topo-hulls">
          {hulls.map((h) => (
            <path
              key={h.key}
              d={h.d}
              fill={h.impure ? 'none' : providerFill(h.provider, 9)}
              stroke={providerColor(h.provider)}
              // Stroke and dashes in screen pixels, like every other piece of
              // chrome here: a 1-unit outline is a 6px band at 6× zoom.
              strokeWidth={1 / scale}
              strokeDasharray={`${(7 / scale).toFixed(2)} ${(6 / scale).toFixed(2)}`}
            />
          ))}
        </g>

        <g className="topo-edges">
          {drawEdges.map(({ le, bow, i }) => {
            const { from, to, edge } = le;
            const geo = edgeGeometry(from, to, bow, ARROW);
            if (!geo) return null;
            const color = edgeColor(edge.kind);
            const dim = selected != null && from.key !== selected && to.key !== selected;
            const flow = edge.kind === EDGE_KINDS.trafficAllow;
            return (
              <g key={i} className="graph-edge" data-dim={dim ? 'true' : undefined}>
                <path
                  d={geo.d}
                  stroke={color}
                  strokeWidth={edgeWidth(edge.count)}
                  // Dashed means heuristic, everywhere, always — the legend
                  // makes that claim and an animated overlay is what keeps
                  // "traffic flows here" from having to borrow the same signal.
                  strokeDasharray={edge.confidence === 'heuristic' ? '5 5' : undefined}
                  markerEnd={`url(#${arrows.get(color)})`}
                />
                {flow && flowDots && (
                  <path className="flow" d={geo.d} stroke={color} strokeWidth={edgeWidth(edge.count) + 1.6} />
                )}
              </g>
            );
          })}
        </g>

        <g className="topo-nodes">
          {layout.nodes.map((n) => {
            const collapsed = n.asset.type === COLLAPSED_NODE_TYPE;
            const color = providerColor(n.asset.provider);
            const dim = selected != null && !neighbours.has(n.key);
            const isSel = n.key === selected;
            const glyph = n.r * 1.3;
            const fs = labelSize(n, scale);
            const labelY = labels.nodes.get(n.key);
            return (
              <g
                key={n.key}
                className="graph-node"
                data-dim={dim ? 'true' : undefined}
                data-selected={isSel ? 'true' : undefined}
                // Hover is styled by CSS, not tracked in state: a setState per
                // pointermove across a 300-node graph re-renders every node
                // and every edge to change one circle's opacity.
                onPointerDown={(e) => {
                  e.stopPropagation();
                  onPointerDown(e, n);
                }}
              >
                {/* First child, which is where SVG wants a <title>. The name is
                    on every node whether or not it is labelled — culling
                    decides what is *drawn*, never what is knowable. The
                    inspector is the other half of that promise. */}
                <title>{`${n.asset.name || n.asset.id} · ${n.asset.type}`}</title>
                <circle className="halo" cx={n.x} cy={n.y} r={n.r + 9} fill={color} />
                {isSel && <circle className="ring" cx={n.x} cy={n.y} r={n.r + 5} />}
                <circle
                  className="body"
                  cx={n.x}
                  cy={n.y}
                  r={n.r}
                  fill={color}
                  fillOpacity={collapsed ? 0.82 : 0.2}
                  stroke={color}
                  strokeWidth={collapsed ? 2 : 1.6}
                />
                {n.r * scale >= GLYPH_MIN_PX && (
                  <use
                    href={`#${uid}-g-${iconKeyForType(n.asset.type)}`}
                    transform={`translate(${n.x - glyph / 2} ${n.y - glyph / 2}) scale(${glyph / 24})`}
                    // AssetIcon strokes with currentColor, so `color` is what
                    // tints the stamped copy.
                    style={{ color: collapsed ? 'var(--bg)' : color }}
                    opacity={collapsed ? 0.9 : 0.85}
                  />
                )}
                {labelY !== undefined && (
                  <text
                    x={n.x}
                    y={labelY}
                    textAnchor="middle"
                    fontSize={fs}
                    fontWeight={collapsed ? 650 : 450}
                    // The halo is painted under the fill (paint-order in the
                    // stylesheet) and so has to be sized here, in graph units,
                    // to come out a constant width on screen.
                    strokeWidth={LABEL_HALO_PX / scale}
                  >
                    {labelText(n)}
                  </text>
                )}
              </g>
            );
          })}
        </g>

        {/* Group labels last: they are the orientation layer, so nothing in the
            diagram is allowed to be drawn over them. */}
        <g className="topo-hull-labels">
          {labels.groups.map((g) => {
            const c = providerColor(g.provider);
            return (
              <g key={g.key}>
                {/* A backing pill rather than a stroke halo: the text sits over
                    hull fills, edges and nodes at once, and a halo only wins
                    against a quiet background. */}
                <rect
                  x={g.box.x0}
                  y={g.box.y0}
                  width={g.box.x1 - g.box.x0}
                  height={g.box.y1 - g.box.y0}
                  rx={(g.box.y1 - g.box.y0) / 2}
                  stroke={c}
                  strokeWidth={1 / scale}
                />
                <text x={g.x} y={g.y} textAnchor="middle" fontSize={g.size} fill={c}>
                  {g.text}
                </text>
              </g>
            );
          })}
        </g>
      </svg>

      <div className="topo-toolbar card glass">
        <NodeSearch
          query={query}
          onQuery={setQuery}
          results={matches.slice(0, MAX_RESULTS)}
          onPick={select}
          selected={selected}
        />
        <div className="topo-zoom">
          <button
            className="btn icon sm"
            type="button"
            onClick={() => zoomBy(1 / 1.35)}
            aria-label="Zoom in"
            title="Zoom in"
          >
            <Icon name="zoom-in" size={14} />
          </button>
          <button
            className="btn icon sm"
            type="button"
            onClick={() => zoomBy(1.35)}
            aria-label="Zoom out"
            title="Zoom out"
          >
            <Icon name="zoom-out" size={14} />
          </button>
          <button className="btn icon sm" type="button" onClick={fit} aria-label="Fit to view" title="Fit to view">
            <Icon name="fit" size={14} />
          </button>
          <button
            className="btn icon sm"
            type="button"
            onClick={reset}
            aria-label="Reset layout"
            title="Reset layout and re-run the simulation"
          >
            <Icon name="target" size={14} />
          </button>
        </div>
        {!settled && (
          <span className="pill live topo-settling">
            <span className="pulse-dot" />
            settling
          </span>
        )}
      </div>

      {/* The legend is also the edge-kind filter, so it is a fixed block that
          can be taller than the canvas it floats over. Collapsing is
          per-session and deliberately not remembered: it is a response to the
          window you have right now, not a preference. */}
      <div className="legend topo-legend" data-open={legendOpen ? 'true' : 'false'}>
        <div className="topo-legend-head">
          <button
            className="topo-legend-toggle"
            type="button"
            aria-expanded={legendOpen}
            aria-controls={legendOpen ? legendId : undefined}
            onClick={() => setLegendOpen((v) => !v)}
          >
            <span className="topo-legend-chevron" aria-hidden="true">
              <Icon name="chevron" size={11} />
            </span>
            <span className="topo-legend-title">Edge kinds</span>
          </button>
          {/* Collapsed, the panel must still say that it is filtering —
              otherwise folding it away silently hides why edges are missing. */}
          {kinds.size > 0 &&
            (legendOpen ? (
              <button className="btn ghost sm" type="button" onClick={onResetKinds}>
                all
              </button>
            ) : (
              <span className="count-badge" title="Edge kinds currently drawn">
                {kinds.size}/{kindCounts.length}
              </span>
            ))}
        </div>
        {legendOpen && (
          <div className="topo-legend-body" id={legendId}>
            {kindCounts.map(([k, n]) => {
              const on = kinds.size === 0 || kinds.has(k);
              const c = edgeColor(k);
              return (
                <button
                  key={k}
                  type="button"
                  className={`chip${on ? ' on' : ''}`}
                  // The label says what the edge means; the title says where
                  // the claim came from, which decides how much to trust it.
                  title={EDGE_SOURCES[k]}
                  aria-pressed={on}
                  style={on ? { color: c, borderColor: c } : undefined}
                  onClick={() => onToggleKind(k)}
                >
                  <span className="dot" style={{ background: c }} />
                  {EDGE_LABELS[k] ?? k}
                  <span className="count-badge">{n.toLocaleString()}</span>
                </button>
              );
            })}
            <p className="hint topo-legend-hint">
              Dashed = heuristic match. Moving dots = traffic a policy permits.
            </p>
          </div>
        )}
      </div>

      <Minimap nodes={layout.nodes} view={view} version={frame} onCenter={centerOn} />

      {sel && <Inspector node={sel} edges={selEdges} onPick={select} onClose={() => setSelected(null)} />}
    </div>
  );
}

// --- inspector -------------------------------------------------------------

interface NeighbourGroup {
  dir: 'out' | 'in';
  kind: string;
  nodes: LayoutNode[];
}

function Inspector({
  node,
  edges,
  onPick,
  onClose,
}: {
  node: LayoutNode;
  edges: LayoutEdge[];
  onPick: (n: LayoutNode) => void;
  onClose: () => void;
}) {
  const a = node.asset;
  const tone = statusTone(a.status);

  const groups = useMemo<NeighbourGroup[]>(() => {
    const m = new Map<string, NeighbourGroup>();
    for (const e of edges) {
      const out = e.from.key === node.key;
      const other = out ? e.to : e.from;
      const key = `${out ? 'out' : 'in'}|${e.edge.kind}`;
      const g = m.get(key);
      if (g) {
        if (!g.nodes.includes(other)) g.nodes.push(other);
      } else {
        m.set(key, { dir: out ? 'out' : 'in', kind: e.edge.kind, nodes: [other] });
      }
    }
    return [...m.values()].sort(
      (x, y) => (x.dir === y.dir ? 0 : x.dir === 'out' ? -1 : 1) || x.kind.localeCompare(y.kind),
    );
  }, [edges, node.key]);

  const outGroups = groups.filter((g) => g.dir === 'out');
  const inGroups = groups.filter((g) => g.dir === 'in');

  return (
    <aside className="inspector topo-inspector" aria-label="Selected node">
      <header>
        <span className="topo-inspector-glyph" style={{ color: providerColor(a.provider) }}>
          <AssetIcon type={a.type} size={16} />
        </span>
        <h3>{a.name || a.id}</h3>
        <button className="btn icon sm ghost" type="button" onClick={onClose} aria-label="Clear selection">
          <Icon name="close" size={13} />
        </button>
      </header>

      <div className="row" style={{ gap: 6 }}>
        <span className="chip" style={{ color: providerColor(a.provider), borderColor: providerColor(a.provider) }}>
          <span className="dot" style={{ background: providerColor(a.provider) }} />
          {a.provider}
        </span>
        {a.status && (
          <span className="pill" style={{ color: toneColor(tone), borderColor: 'currentColor' }}>
            {a.status}
          </span>
        )}
      </div>

      <div className="mono faint topo-inspector-id">{a.id}</div>

      <dl>
        <dt>Type</dt>
        <dd className="mono">{a.type}</dd>
        {a.region && (
          <>
            <dt>Region</dt>
            <dd>{a.region}</dd>
          </>
        )}
        {a.account_id && (
          <>
            <dt>Account</dt>
            <dd className="mono">{a.account_id}</dd>
          </>
        )}
        <dt>Edges</dt>
        <dd>{edges.length}</dd>
        {/* The key belongs on the Fragment, not on the dt/dd inside it — a
            keyless Fragment in a map is exactly what React warns about. <dl>
            only permits dt/dd children, so a keyed <div> wrapper is not an
            option here. */}
        {Object.entries(a.tags ?? {})
          .slice(0, 14)
          .map(([k, v]) => (
            <Fragment key={k}>
              <dt>{k}</dt>
              <dd className="mono">{truncate(v, 60)}</dd>
            </Fragment>
          ))}
      </dl>

      {groups.length > 0 && (
        <div className="neighbours">
          {outGroups.length > 0 && <NeighbourSection title="reaches" groups={outGroups} onPick={onPick} />}
          {inGroups.length > 0 && <NeighbourSection title="reached by" groups={inGroups} onPick={onPick} />}
        </div>
      )}
    </aside>
  );
}

function NeighbourSection({
  title,
  groups,
  onPick,
}: {
  title: string;
  groups: NeighbourGroup[];
  onPick: (n: LayoutNode) => void;
}) {
  return (
    <section className="topo-neighbours">
      <h4>{title}</h4>
      {groups.map((g) => (
        <div key={`${g.dir}|${g.kind}`} className="topo-neighbour-group">
          <span className="topo-neighbour-kind" style={{ color: edgeColor(g.kind) }}>
            <span className="dot" style={{ background: edgeColor(g.kind) }} />
            {EDGE_LABELS[g.kind] ?? g.kind}
          </span>
          {g.nodes.map((n) => (
            <button
              key={n.key}
              type="button"
              className="asset-chip topo-neighbour"
              onClick={() => onPick(n)}
              title={n.asset.id}
            >
              <span className="dot" style={{ background: providerColor(n.asset.provider) }} />
              <strong>{truncate(n.asset.name || n.asset.id, 30)}</strong>
            </button>
          ))}
        </div>
      ))}
    </section>
  );
}

// --- search ----------------------------------------------------------------

const MAX_RESULTS = 8;

/**
 * Type-to-find over node names and ids.
 *
 * Reuses the command palette's subsequence matcher rather than a substring
 * test: node names are dotted and slashed ("prod/api-gateway",
 * "api.example.com"), and a subsequence match is what lets "pag" find the
 * first without knowing where the separators fall. The matching itself lives
 * in the canvas — see `matches` there — because the label pass needs the same
 * answer; this component is the input and the dropdown.
 */
function NodeSearch({
  query,
  onQuery,
  results,
  onPick,
  selected,
}: {
  query: string;
  onQuery: (q: string) => void;
  results: LayoutNode[];
  onPick: (n: LayoutNode) => void;
  selected: string | null;
}) {
  const [open, setOpen] = useState(false);
  const [active, setActive] = useState(0);
  const listId = `${useId().replace(/:/g, '')}-results`;

  const pick = (n: LayoutNode) => {
    onPick(n);
    setOpen(false);
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
      e.preventDefault();
      if (results.length === 0) return;
      setOpen(true);
      setActive((i) => (i + (e.key === 'ArrowDown' ? 1 : results.length - 1)) % results.length);
    } else if (e.key === 'Enter') {
      const hit = results[active] ?? results[0];
      if (hit) {
        e.preventDefault();
        pick(hit);
      }
    } else if (e.key === 'Escape') {
      if (open) {
        e.preventDefault();
        setOpen(false);
      }
    }
  };

  const expanded = open && results.length > 0;

  return (
    <div className="topo-search">
      <Icon name="search" size={13} />
      <input
        type="search"
        value={query}
        placeholder="Find a node…"
        aria-label="Find a node by name or id"
        role="combobox"
        aria-expanded={expanded}
        // Only while the listbox exists: aria-controls pointing at an absent
        // id is a dangling reference, not a hint.
        aria-controls={expanded ? listId : undefined}
        aria-autocomplete="list"
        aria-activedescendant={expanded ? `${listId}-${active}` : undefined}
        onChange={(e) => {
          onQuery(e.target.value);
          setActive(0);
          setOpen(true);
        }}
        onFocus={() => setOpen(true)}
        // Blur closes on the next tick so a click on a result still lands.
        onBlur={() => window.setTimeout(() => setOpen(false), 120)}
        onKeyDown={onKeyDown}
      />
      {query && (
        <button
          className="btn icon sm ghost"
          type="button"
          aria-label="Clear search"
          onClick={() => {
            onQuery('');
            setOpen(false);
          }}
        >
          <Icon name="close" size={12} />
        </button>
      )}
      {expanded && (
        <ul className="topo-results" id={listId} role="listbox" aria-label="Matching nodes">
          {results.map((n, i) => (
            // mousedown, not click: the input's blur fires first otherwise and
            // the list is gone before the click can land on it.
            <li
              key={n.key}
              id={`${listId}-${i}`}
              role="option"
              aria-selected={i === active}
              data-active={i === active ? 'true' : undefined}
              data-current={n.key === selected ? 'true' : undefined}
              onMouseDown={(e) => {
                e.preventDefault();
                pick(n);
              }}
              onMouseEnter={() => setActive(i)}
            >
              <span className="dot" style={{ background: providerColor(n.asset.provider) }} />
              <span className="truncate">{n.asset.name || n.asset.id}</span>
              <span className="mono faint truncate">{n.asset.type}</span>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}

// --- labels ----------------------------------------------------------------
//
// Drawing all 152 labels of the demo inventory at once turns the middle of the
// picture into text soup, so most frames draw a subset. Everything that
// decides which subset lives here.

/**
 * Label sizes, in *screen* pixels — converted to graph units (`px / scale`) at
 * draw time. This is the change the rest of the culling rests on.
 *
 * A font size fixed in graph units, which is what an SVG `font-size` attribute
 * inside a viewBox means, scales with the zoom: two labels that collide at one
 * zoom collide at every zoom, and "zoom in to read it" hands you a larger copy
 * of the same collision. Fixing the size on screen instead makes a label's
 * footprint *shrink* in graph space as the view tightens, so the placement
 * pass below admits more of them the further in you go — the requested "zoom
 * in and more labels appear", arrived at by geometry rather than by a rule
 * bolted on top of one.
 *
 * It also retires the old `LABEL_MIN_SCALE` cutoff, which existed because
 * zooming out shrank 11 units into four pixels of grey mush. Eleven pixels are
 * eleven pixels now; what zooming out costs is *room*, and room is what the
 * budget and the overlap test already ration.
 */
const LABEL_PX = 11;
const COLLAPSED_LABEL_PX = 14;
const GROUP_LABEL_PX = 12.5;
/** Width of the halo painted under a node label (paint-order: stroke). */
const LABEL_HALO_PX = 3.4;

/**
 * Mean glyph advance as a fraction of the font size.
 *
 * Estimated, never measured: `getComputedTextLength` forces a layout per call,
 * and this runs for every candidate on every settle frame — measuring 152
 * labels at 60fps is not affordable, and at the 900-node ceiling it is absurd.
 * 0.55em is a shade generous for the system UI sans at these sizes (the dotted,
 * dashed, mostly-lowercase ASCII of asset names runs nearer 0.5), and generous
 * is the safe direction: over-estimating costs a label that would have fitted,
 * under-estimating ships the overlap this pass exists to prevent.
 */
const CHAR_W = 0.55;

/**
 * The unpinned label budget: labels per million screen pixels, damped by zoom.
 *
 * The overlap test alone would already thin things out, but on a well-spread
 * graph it would happily accept three hundred non-colliding labels — each
 * legible, and collectively unreadable. This is the cap that says the diagram
 * is for reading; the search box and the inspector are for looking things up.
 *
 * √scale rather than scale: a 4× zoom shows a sixteenth of the graph's area,
 * so granting 4× the labels holds the on-screen density roughly level while
 * still paying out for the zoom. The clamp stops a 100× zoom asking for a
 * thousand labels, and the floor keeps the hubs named at any zoom-out.
 */
const LABEL_DENSITY = 26;
const LABEL_BUDGET_MIN = 6;
const LABEL_BUDGET_MAX = 120;

function labelBudget(w: number, h: number, scale: number): number {
  const zoom = Math.sqrt(clamp(scale, 0.25, 9));
  return Math.round(
    clamp(((w * h) / 1e6) * LABEL_DENSITY * zoom, LABEL_BUDGET_MIN, LABEL_BUDGET_MAX),
  );
}

/**
 * Search hits that get a guaranteed label.
 *
 * Capped, because `match` is a *subsequence* test: a one-character query is a
 * subsequence of very nearly every node name, so an uncapped "always label a
 * match" would rebuild the soup under another name — and would do it while the
 * user was typing, which is the worst possible moment.
 */
const MAX_PINNED_MATCHES = 24;

interface Box {
  x0: number;
  y0: number;
  x1: number;
  y1: number;
}

/** A group label that survived placement, with the pill drawn behind it. */
interface PlacedGroupLabel {
  key: string;
  text: string;
  provider: string;
  /** Text anchor: horizontal centre, baseline. */
  x: number;
  y: number;
  size: number;
  box: Box;
}

interface Labels {
  /** Node key → the baseline its label is drawn at. Usually below the node;
   *  see the flip in {@link placeLabels}. */
  nodes: Map<string, number>;
  groups: PlacedGroupLabel[];
}

function overlaps(a: Box, b: Box): boolean {
  return a.x0 < b.x1 && a.x1 > b.x0 && a.y0 < b.y1 && a.y1 > b.y0;
}

/** The box a centred single-line label occupies, in graph units. `cy` is the
 *  baseline; the asymmetry about it is cap height above, descenders below. */
function textBox(cx: number, cy: number, text: string, fs: number, gap: number): Box {
  const half = (text.length * fs * CHAR_W) / 2;
  return {
    x0: cx - half - gap,
    x1: cx + half + gap,
    y0: cy - fs * 0.82 - gap * 0.5,
    y1: cy + fs * 0.26 + gap * 0.5,
  };
}

/** What a node's label says. Truncated — the untruncated name is on the node's
 *  `<title>` and in the inspector, which is where a name too long to draw is
 *  supposed to be read. */
function labelText(n: LayoutNode): string {
  return truncate(n.asset.name || n.asset.id, n.asset.type === COLLAPSED_NODE_TYPE ? 40 : 26);
}

function labelSize(n: LayoutNode, scale: number): number {
  return (n.asset.type === COLLAPSED_NODE_TYPE ? COLLAPSED_LABEL_PX : LABEL_PX) / scale;
}

/** Baseline for a node's label: clear of the circle by a screen-constant gap,
 *  so it neither drifts away as you zoom in nor lands on the node as you zoom
 *  out. */
function labelBaseline(n: LayoutNode, fs: number): number {
  return n.y + n.r + fs * 1.05;
}

/**
 * How a node ranks when there is not room for every label.
 *
 * Degree, or membership for a collapsed node — the two never really mix, since
 * `detail: low` produces no collapsed nodes and the other levels produce
 * nothing else, so the base is just a guarantee that a node standing for four
 * thousand assets outranks one with eleven edges if they ever meet.
 *
 * Degree is counted over the whole graph, not over the edge kinds currently
 * drawn: toggling a legend chip should change which *edges* are on screen, not
 * reshuffle every label on the canvas.
 */
const COLLAPSED_RANK_BASE = 1e6;

function importance(n: LayoutNode): number {
  const members = Number(n.asset.tags?.member_count ?? 0);
  return members > 0 ? COLLAPSED_RANK_BASE + members : n.degree;
}

/**
 * Decides which labels this frame can carry, in one greedy pass over a shared
 * set of accepted boxes.
 *
 * The rule, in priority order:
 *
 *  1. **Group labels first.** They are the orientation layer — "which cloud am
 *     I looking at" is the last thing worth saying when everything else has
 *     been culled — so they claim their space before anything else, and a
 *     group whose label would land on another group's simply goes unlabelled
 *     rather than stacking two names in one place (which is what put
 *     "kubernetes · 52" on top of "cloudflare · 30").
 *  2. **Pinned node labels**: the selection, everything one edge from it, and
 *     the current search hits. These are the nodes the operator has just asked
 *     about, so they are exempt from the budget — but not from the overlap
 *     test, because two pinned labels drawn on top of each other answer
 *     nobody.
 *  3. **Everything else by importance**, until the budget runs out.
 *
 * A candidate is rejected outright if its box touches one already accepted;
 * there is no attempt to nudge it somewhere else. Displacing a label breaks
 * the one thing a label has to keep — being unambiguously attached to its own
 * node — and a leader-line layout is a different, much larger feature.
 *
 * Cost is O(candidates × accepted), with `accepted` bounded by the budget plus
 * the pins: about 140k rectangle tests at the page's 900-node ceiling, which
 * is noise beside the O(n²) force pass that produced the positions. Off-screen
 * candidates are dropped before any of it.
 */
function placeLabels(
  nodes: readonly LayoutNode[],
  byKey: ReadonlyMap<string, LayoutNode>,
  hulls: readonly Hull[],
  pinned: readonly string[],
  view: View,
  scale: number,
  budget: number,
): Labels {
  const taken: Box[] = [];
  const fits = (b: Box): boolean => {
    for (const t of taken) if (overlaps(b, t)) return false;
    return true;
  };

  // A margin, so a label anchored just off-screen still blocks the space its
  // box reaches into rather than letting an on-screen label overlap it.
  const inView = (x: number, y: number): boolean =>
    x > view.x - view.w * 0.15 &&
    x < view.x + view.w * 1.15 &&
    y > view.y - view.h * 0.15 &&
    y < view.y + view.h * 1.15;

  const groups: PlacedGroupLabel[] = [];
  const gfs = GROUP_LABEL_PX / scale;
  for (const h of hulls) {
    // Pushed out along the ray from the blob's centre through its topmost
    // vertex, so the label flies as a flag off the top of the shape instead of
    // sitting on the nodes crowded against its upper edge.
    const x = h.topX + h.ux * gfs * 1.7;
    const y = h.topY + h.uy * gfs * 1.7;
    if (!inView(x, y)) continue;
    const box = textBox(x, y, h.label, gfs, gfs * 0.55);
    if (!fits(box)) continue;
    taken.push(box);
    groups.push({ key: h.key, text: h.label, provider: h.provider, x, y, size: gfs, box });
  }

  const drawn = new Map<string, number>();
  const place = (n: LayoutNode, flip = false): boolean => {
    const fs = labelSize(n, scale);
    const text = labelText(n);
    // Below the node is where a label belongs and where the eye goes looking
    // for it, so it is always tried first. A *pinned* label gets one fallback
    // above the node rather than going undrawn: two adjacent nodes both label
    // downwards and collide, which was losing half the neighbourhood of a
    // dense hub to its own neighbours. One alternative, not a search — a label
    // that wanders far from its node has stopped naming it.
    const tries = flip ? [labelBaseline(n, fs), n.y - n.r - fs * 0.4] : [labelBaseline(n, fs)];
    for (const cy of tries) {
      if (!inView(n.x, cy)) continue;
      const box = textBox(n.x, cy, text, fs, fs * 0.34);
      if (!fits(box)) continue;
      taken.push(box);
      drawn.set(n.key, cy);
      return true;
    }
    return false;
  };

  for (const k of pinned) {
    const n = byKey.get(k);
    if (n) place(n, true);
  }

  // A copy, because sorting `layout.nodes` in place would reorder the render
  // and the minimap behind the simulation's back.
  const rest = nodes
    .filter((n) => !drawn.has(n.key) && inView(n.x, n.y))
    // Array#sort is stable, so equal-importance nodes keep the server's
    // ordering and the same graph labels the same nodes on every frame.
    .sort((a, b) => importance(b) - importance(a));

  let left = budget;
  for (const n of rest) {
    if (left <= 0) break;
    // A pinned node that failed the overlap test lands here again and fails
    // again — costing one rectangle test, and no budget.
    if (place(n)) left--;
  }

  return { nodes: drawn, groups };
}

// --- helpers ---------------------------------------------------------------

/** Collapsed edges carry a count; a linear width would make a ×400 edge a
 *  200px band, so it is logarithmic and capped. */
function edgeWidth(count: number | undefined): number {
  return count && count > 1 ? Math.min(6, 1.2 + Math.log2(count) * 0.9) : 1.4;
}

function dominantProvider(nodes: LayoutNode[]): string {
  const tally = new Map<string, number>();
  for (const n of nodes) tally.set(n.asset.provider, (tally.get(n.asset.provider) ?? 0) + 1);
  let best = '';
  let bestN = -1;
  for (const [p, n] of tally) {
    if (n > bestN || (n === bestN && p < best)) {
      best = p;
      bestN = n;
    }
  }
  return best;
}

/** Grows `box` to `aspect` around its own centre, so the fitted graph is never
 *  cropped by the letterboxing the viewBox would otherwise apply. */
function frameToAspect(
  box: { minX: number; minY: number; maxX: number; maxY: number },
  aspect: number,
): View {
  const cx = (box.minX + box.maxX) / 2;
  const cy = (box.minY + box.maxY) / 2;
  const w = Math.max(box.maxX - box.minX, (box.maxY - box.minY) * aspect, MIN_VIEW_W);
  const h = w / aspect;
  return { x: cx - w / 2, y: cy - h / 2, w, h };
}

function clamp(n: number, lo: number, hi: number): number {
  return Math.min(hi, Math.max(lo, n));
}

function truncate(s: string, n: number): string {
  return s.length <= n ? s : `${s.slice(0, n - 1)}…`;
}

/**
 * Longest group key a label pill may carry, in characters.
 *
 * Group keys are not names. Under group-by account they are opaque
 * identifiers — a 32-hex Cloudflare account, a 36-char UUID, a 44-char OCID —
 * and drawn whole at GROUP_LABEL_PX one of them is a 350px banner: a quarter
 * of the canvas, anchored to a blob it is far too wide to belong to, sitting
 * across two other clusters. Measured on the demo inventory, which is exactly
 * the picture this cull exists to keep readable.
 */
const GROUP_KEY_MAX = 24;

/**
 * Cuts `s` to `n` characters from the *middle*.
 *
 * From the middle rather than the end because of what these strings are:
 * `ocid1.tenancy.oc1..aaaaaaaanorthwindroot0001` is nineteen characters of
 * constant prefix, so a head-only cut names every OCI tenancy identically. Both
 * ends is what distinguishes them — and it costs nothing at group-by provider,
 * where the keys are short words and this returns them untouched.
 */
function elideMiddle(s: string, n: number): string {
  if (s.length <= n) return s;
  const head = Math.ceil((n - 1) / 2);
  return `${s.slice(0, head)}…${s.slice(s.length - (n - 1 - head))}`;
}
