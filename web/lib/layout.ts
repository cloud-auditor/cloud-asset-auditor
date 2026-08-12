import type { Point } from './hull';
import type { Asset, Edge } from './types';

export interface LayoutNode {
  key: string;
  asset: Asset;
  x: number;
  y: number;
  vx: number;
  vy: number;
  /** Node radius, scaled by how much the node represents. */
  r: number;
  degree: number;
  /** Index into {@link Layout.groups}, or -1 when the layout is flat. */
  gi: number;
  fixed?: boolean;
}

export interface LayoutEdge {
  from: LayoutNode;
  to: LayoutNode;
  edge: Edge;
}

/**
 * One cluster of same-group nodes, and the state the group forces need.
 *
 * The seeded fields are fixed for the life of the layout so {@link reseed} can
 * restore the exact starting picture; the live fields are recomputed by every
 * {@link tick} because the nodes move.
 */
export interface LayoutGroup {
  key: string;
  members: LayoutNode[];
  /** Deterministic starting centre, and the blob radius it was sized for. */
  seedX: number;
  seedY: number;
  seedR: number;
  /** Live centroid. */
  cx: number;
  cy: number;
  /** Live radius estimate — how much canvas the cluster is currently using. */
  radius: number;
}

export interface Layout {
  nodes: LayoutNode[];
  edges: LayoutEdge[];
  byKey: Map<string, LayoutNode>;
  /** The dimension nodes were clustered by; '' when the layout is flat. */
  groupBy: string;
  /** Clusters, ordered by key. Empty when the layout is flat. */
  groups: LayoutGroup[];
  /** Ticks already spent by the synchronous warm-up — where an animated
   *  settle loop should pick up on the {@link alphaAt} schedule. */
  warmTicks: number;
}

/** Canonical node key. Mirrors `refKey` in internal/topology/topology.go. */
export function refKey(r: { provider: string; id: string }): string {
  return `${r.provider}/${r.id}`;
}

/**
 * A deterministic 32-bit hash, used to seed initial node positions.
 *
 * Determinism is the point: `Math.random()` would give a different layout on
 * every render, so the diagram would visibly reshuffle whenever React
 * re-rendered — and two people looking at the same inventory would see
 * different pictures. Seeding from the node key means the same graph always
 * lays out the same way.
 *
 * Group placement leans on the same guarantee from the other end: group
 * centres are ordered by group *name*, so the ring of clusters is stable too
 * — not just each node within its cluster.
 */
function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) {
    h ^= s.charCodeAt(i);
    h = Math.imul(h, 16777619);
  }
  return h >>> 0;
}

/**
 * Where a node starts before any force is applied: somewhere on a ring of
 * `inner`…`inner + span` around (cx, cy), chosen by its hash.
 *
 * Split out of buildLayout so {@link reseed} can put a graph the user has
 * dragged around back exactly where it began — a "reset" that landed somewhere
 * new would not be a reset.
 *
 * A ring rather than a filled disc: it gives the force pass a spread-out
 * starting state, so it converges without the initial "everything explodes out
 * of the centre" frame.
 */
function seedAt(key: string, cx: number, cy: number, inner: number, span: number): Point {
  const h = hash(key);
  const angle = ((h % 3600) / 3600) * Math.PI * 2;
  // The high bits pick the band offset; the low bits already went to the
  // angle, and reusing them would correlate radius with direction into a
  // visible spiral.
  const radius = inner + ((h >>> 12) % 100) * (span / 100);
  return { x: cx + Math.cos(angle) * radius, y: cy + Math.sin(angle) * radius };
}

/** The flat seed ring: the exact formula every node used before grouping
 *  existed, so an ungrouped layout still starts from the identical state. */
function flatSeed(key: string, width: number, height: number): Point {
  return seedAt(key, width / 2, height / 2, 0.18 * Math.min(width, height), 90);
}

// --- force constants -------------------------------------------------------
//
// The numbers below are quoted against the demo inventory (152 nodes in a
// 1200×760 field), where the natural node spacing k works out at ~48 units.
// They are ratios, not absolutes: k scales with the field and the node count,
// and every force here is expressed in terms of it or of a live distance.

/** Pull toward the canvas centre, applied per node, for a flat layout. */
const GRAVITY_FLAT = 0.012;

/**
 * Pull toward the canvas centre for a grouped layout — applied to the *group*
 * (every member gets the group's correction) rather than to each node.
 *
 * Per-node gravity would squeeze each cluster toward the middle and fight the
 * separation force for the whole run; correcting the centroid moves a cluster
 * without compressing it.
 *
 * Weaker than the flat figure because it is what balances separation: raise it
 * to the flat 0.012 and the clusters are dragged into each other before the
 * two forces cancel — twice the overlapping hull pairs on the demo inventory,
 * 4/10 against 2/10. Not weaker still, because gravity is also the only thing
 * anchoring the arrangement: at 0.004 the picture wanders half again as far
 * when the simulation is driven past its cooling schedule.
 */
const GRAVITY_GROUPED = 0.008;

/**
 * Pull toward the node's own group centroid.
 *
 * Deliberately weak against edge attraction, which is (d²/k)/14 — at 100 units
 * that is ~14.9 against cohesion's 4.5, and the gap *widens* with distance
 * because attraction grows quadratically where cohesion is linear. So a node
 * whose edges cross a group boundary gets dragged to the rim of its cluster
 * and sits between the two, which is exactly where the cross-cloud joins are
 * and the single most interesting thing this diagram has to say. Cohesion only
 * has to win for nodes with *no* outside pull, and for those it is unopposed.
 *
 * The balance is checked rather than asserted: on the demo inventory a node
 * with a cross-group edge settles about twice as far from its own centroid as
 * one without (2.0× at group-by provider, 2.2× at account). If a change to
 * these weights pushes that ratio toward 1, cohesion has started swallowing
 * the joins and the diagram has stopped saying anything about them.
 */
const COHESION = 0.045;

/**
 * Push between overlapping group centroids, applied to every member of both.
 *
 * Applying it to the whole cluster translates it rather than distorting it, so
 * cohesion does not have to repair the shape afterwards. It is resisted only
 * by the cross-group edges: a 52-node cluster contributes 52 × push, where
 * three edges crossing its boundary contribute three × attraction — so groups
 * separate and the boundary nodes stretch out toward their neighbours instead
 * of the cluster being held hostage by one join.
 *
 * Kept gentle, because this is the least stable force in the file: it is the
 * only one whose own target moves as a result of applying it, so pushed hard
 * the clusters chase each other instead of settling. Turning it off entirely
 * cuts the post-settle drift tenfold (262 → 25 px) but nearly doubles the
 * overlapping hull pairs under group-by account (6/21 → 11/21). This value is
 * where the churn is affordable and the blobs still come apart — and the churn
 * is only ever theoretical in the app, which stops ticking when the schedule
 * ends rather than holding the simulation hot.
 */
const SEPARATION = 0.04;

/** Clear space demanded between two group blobs, on top of their radii. Sized
 *  against the hull padding in GraphCanvas (HULL_PAD, 34 at the time of
 *  writing): two hulls each reach a pad beyond their outermost node, so
 *  anything under 2 × 34 leaves them touching even when their nodes do not. */
const GROUP_GAP = 84;

/**
 * Repulsion multiplier between nodes in *different* groups.
 *
 * Surface tension at the cluster boundary, and the one force that addresses
 * the failure cohesion cannot: a node does not have to be far from its own
 * centroid to be in the wrong place, it has to be inside somebody else's
 * cluster — which is how a hull ends up drawn around a node that is not a
 * member. Extra repulsion pushes foreign nodes back out through the boundary
 * while leaving their edge attraction to hold them against the outside of it,
 * so the join stays visible and the blob stays honest.
 */
const CROSS_GROUP_REPULSION = 1.8;

/** Ceiling on a cluster's separation radius, as a multiple of its seeded one.
 *  A group cannot claim unbounded space because one of its members was pulled
 *  across the picture by a cross-cloud edge — see {@link measureGroups}. */
const GROUP_MAX_GROWTH = 2.2;

/**
 * Beyond this many groups the pairwise separation pass costs more than it
 * buys. Group-by account on a large tenancy can produce hundreds of two-node
 * groups; the seeding ring already places those without overlap, and O(g²)
 * with g in the hundreds is a second O(n²) pass in the frame budget.
 */
const MAX_SEPARATED_GROUPS = 64;

/** A cluster's seeded blob radius, as a multiple of k√members. Area per node
 *  is what makes clusters scale sanely: √count, not count, or one big group
 *  would be seeded a screen wide. 0.62 is the equal-area figure (1/√π ≈ 0.56)
 *  plus slack, since a settled cluster is never a perfect disc. */
const GROUP_SEED_SPREAD = 0.62;

/** Damping applied to velocity each tick. */
const DAMPING = 0.82;

/**
 * Per-tick step ceiling, as a multiple of the natural spacing k.
 *
 * The fixed 24 this replaces was wrong in both directions — it let a sparse
 * graph crawl and a dense one fling — where half the natural spacing means
 * "never jump past your own neighbour" at any size. It earns the change at the
 * top end: on a 900-node graph the worst single-tick jump during the *visible*
 * part of the settle drops from 15.4px to 6.3px, and a 900-node graph is
 * precisely the one whose warm-up budget leaves most of the settle on screen.
 *
 * At the demo's k≈48 it computes to 24.01 against the old 24. That is not the
 * same number, and this simulation is chaotic enough that 0.05% compounds into
 * a visibly different (equally valid) arrangement of the same flat graph — so
 * an ungrouped layout is the same *simulation* as before, not the same pixels.
 */
const MAX_STEP_K = 0.5;
const MAX_STEP_CEILING = 28;

// --- convergence -----------------------------------------------------------

/** Ticks in a complete settle. */
export const SETTLE_TICKS = 300;

/** Floor under the cooling factor: the last ticks still nudge, so a graph that
 *  is nearly right finishes rather than freezing mid-adjustment. */
const ALPHA_FLOOR = 0.02;

/**
 * Cooling factor for tick `i` of a settle.
 *
 * Quadratic rather than the linear ramp this replaced. Linear cooling spends
 * half its budget above alpha 0.5, so the graph is still taking large steps
 * two thirds of the way through — which is why the "settling" pill used to
 * outlast anyone's patience and a screenshot caught it mid-move. Squaring
 * front-loads the movement into the warm-up (which nobody watches) and leaves
 * a short, visibly calm tail.
 */
export function alphaAt(i: number): number {
  const t = i <= 0 ? 0 : i >= SETTLE_TICKS ? 1 : i / SETTLE_TICKS;
  const rem = 1 - t;
  return ALPHA_FLOOR + (1 - ALPHA_FLOOR) * rem * rem;
}

/**
 * Fraction of the settle {@link buildLayout} runs synchronously.
 *
 * Not all of it: the remaining ticks are what the caller animates, and a
 * diagram that pops into its final state fully formed reads as a static image
 * — the brief settle is what tells the operator the picture was *computed*
 * from their inventory rather than fetched. Sixty percent is where the
 * quadratic schedule has done all the large motion (alpha is down to 0.16),
 * so the tail polishes instead of untangling.
 */
const WARMUP_FRACTION = 0.6;

/**
 * Ceiling on the warm-up, in pair interactions.
 *
 * The warm-up is a synchronous block inside a React render, so its cost is a
 * frozen tab — this is the number that stops a big graph from hanging one.
 * A tick is n²/2 pair interactions, measured at ~3.0 ns each on an M-series
 * laptop, so the budget buys, by graph size:
 *
 *   152 nodes (the demo)  →   11.5k pairs/tick  →  fraction wins, 180 ticks,  7 ms
 *   400 nodes             →   80k  pairs/tick   →  fraction wins, 180 ticks, 43 ms
 *   600 nodes             →  180k  pairs/tick   →  budget wins,   100 ticks, 52 ms
 *   900 nodes (the cap)   →  405k  pairs/tick   →  budget wins,    44 ticks, 55 ms
 *
 * So the worst case is ~55 ms here, and something like 165 ms on a machine
 * three times slower — noticeable, under the ~300 ms that reads as a hang, and
 * spent once on a page that has just waited on a network audit. 900 is the
 * hard ceiling: the topology page refuses to render more (MAX_RENDERED_NODES)
 * and steers to `--detail medium|high`.
 *
 * Above ~500 nodes the budget bites and more of the settle is left to the
 * animated tail, which is the right trade — that is also where a frame costs
 * enough that the tail reads as progress rather than as jitter.
 */
const WARMUP_PAIR_BUDGET = 18_000_000;

/** How many ticks the warm-up can afford for a graph this size. */
function warmupTicks(n: number): number {
  const perTick = Math.max(1, (n * (n - 1)) / 2);
  const wanted = Math.round(SETTLE_TICKS * WARMUP_FRACTION);
  return Math.max(8, Math.min(wanted, Math.floor(WARMUP_PAIR_BUDGET / perTick)));
}

// --- build -----------------------------------------------------------------

export function buildLayout(
  nodes: Asset[],
  edges: Edge[],
  width: number,
  height: number,
  /** Cluster nodes by this dimension; '' lays the graph out flat. */
  groupBy = '',
): Layout {
  const byKey = new Map<string, LayoutNode>();
  const degree = new Map<string, number>();

  for (const e of edges) {
    const f = refKey(e.from);
    const t = refKey(e.to);
    degree.set(f, (degree.get(f) ?? 0) + 1);
    degree.set(t, (degree.get(t) ?? 0) + 1);
  }

  const out: LayoutNode[] = [];
  const groups: LayoutGroup[] = [];
  const groupIndex = new Map<string, number>();

  for (const a of nodes) {
    const key = refKey(a);
    if (byKey.has(key)) continue; // defensive: the API should not repeat a node
    const deg = degree.get(key) ?? 0;
    const members = Number(a.tags?.member_count ?? 0);
    const n: LayoutNode = {
      key,
      asset: a,
      x: 0,
      y: 0,
      vx: 0,
      vy: 0,
      // Collapsed nodes size with their membership so a 4,000-asset platform
      // reads as bigger than a 3-asset one; plain nodes size with degree.
      r: members > 0 ? 16 + Math.min(26, Math.log2(members + 1) * 4.5) : 8 + Math.min(10, deg * 1.1),
      degree: deg,
      gi: -1,
    };
    if (groupBy) {
      const g = groupOf(a, groupBy);
      let gi = groupIndex.get(g);
      if (gi === undefined) {
        gi = groups.length;
        groupIndex.set(g, gi);
        groups.push({ key: g, members: [], seedX: 0, seedY: 0, seedR: 0, cx: 0, cy: 0, radius: 0 });
      }
      n.gi = gi;
      groups[gi].members.push(n);
    }
    byKey.set(key, n);
    out.push(n);
  }

  // Sorted by name, and the indices re-pointed to match: group centres are
  // placed in this order, so the ring is a function of the group *names* and
  // not of which asset happened to stream in first.
  groups.sort((a, b) => (a.key < b.key ? -1 : a.key > b.key ? 1 : 0));
  for (let i = 0; i < groups.length; i++) for (const n of groups[i].members) n.gi = i;

  const le: LayoutEdge[] = [];
  for (const e of edges) {
    const from = byKey.get(refKey(e.from));
    const to = byKey.get(refKey(e.to));
    // An edge to a node the server filtered out has nothing to draw between.
    if (from && to && from !== to) le.push({ from, to, edge: e });
  }

  const layout: Layout = { nodes: out, edges: le, byKey, groupBy, groups, warmTicks: 0 };
  placeGroups(layout, width, height);
  reseed(layout, width, height);
  return layout;
}

/**
 * Fixes each group's seed centre on a ring around the canvas centre.
 *
 * Two properties matter. **Deterministic**: centres come from the sorted group
 * names and the member counts, nothing else, so the same inventory always
 * draws the same picture — the guarantee {@link hash} makes for nodes, made
 * for clusters. **Non-overlapping at rest**: each group gets an arc in
 * proportion to its blob radius, then the ring is widened until the chord
 * between every adjacent pair of centres clears both their radii plus
 * {@link GROUP_GAP}. Seeding clusters already apart is worth far more than any
 * amount of force tuning — the separation force then has to fix rounding
 * rather than untangle a pile, which is most of why this converges quickly.
 *
 * Deriving the radius from the perimeter alone (`Σ2r / 2π`) is the textbook
 * form and it fails at small g: with two groups it puts the centres at
 * 0.7 × (r₁+r₂), i.e. overlapping. The explicit adjacent-chord solve below is
 * correct for two groups and for twenty.
 */
function placeGroups(layout: Layout, width: number, height: number): void {
  const { groups, nodes } = layout;
  if (groups.length === 0) return;

  const k = spacing(nodes.length, width, height);
  const cx = width / 2;
  const cy = height / 2;

  for (const g of groups) g.seedR = GROUP_SEED_SPREAD * k * Math.sqrt(g.members.length);

  if (groups.length === 1) {
    groups[0].seedX = cx;
    groups[0].seedY = cy;
    return;
  }

  // Arc length in proportion to blob radius, so a 52-node cluster is given
  // more of the circle than a 1-node one.
  const span = groups.reduce((s, g) => s + 2 * g.seedR, 0) || 1;
  const angles: number[] = [];
  let acc = 0;
  for (const g of groups) {
    angles.push((2 * Math.PI * (acc + g.seedR)) / span);
    acc += 2 * g.seedR;
  }

  let R = 0;
  for (let i = 0; i < groups.length; i++) {
    const j = (i + 1) % groups.length;
    // Wrap the last gap back through 2π rather than reading it as negative.
    const dTheta = j === 0 ? 2 * Math.PI - angles[i] + angles[0] : angles[j] - angles[i];
    const chord = 2 * Math.sin(Math.max(dTheta, 1e-3) / 2);
    R = Math.max(R, (groups[i].seedR + groups[j].seedR + GROUP_GAP) / chord);
  }

  for (let i = 0; i < groups.length; i++) {
    groups[i].seedX = cx + Math.cos(angles[i]) * R;
    groups[i].seedY = cy + Math.sin(angles[i]) * R;
  }
}

/**
 * Returns every node to its seeded position, drops pins and momentum, and
 * re-runs the warm-up, so a caller can restart the simulation from the
 * identical starting state.
 */
export function reseed(layout: Layout, width: number, height: number): void {
  for (const n of layout.nodes) {
    const g = n.gi >= 0 ? layout.groups[n.gi] : null;
    // Within a cluster the band is a fraction of the cluster's own radius, so
    // seeding is self-similar: the same picture at whatever scale the group's
    // size demands.
    const p = g
      ? seedAt(n.key, g.seedX, g.seedY, g.seedR * 0.45, g.seedR * 0.55)
      : flatSeed(n.key, width, height);
    n.x = p.x;
    n.y = p.y;
    n.vx = 0;
    n.vy = 0;
    n.fixed = false;
  }
  layout.warmTicks = 0;
  warmUp(layout, width, height);
}

/**
 * Runs the front of the settle synchronously, before anything is painted.
 *
 * Without it the first frame on screen is the seed ring, and the user watches
 * the graph untangle from a shape that means nothing. The cost is bounded by
 * {@link WARMUP_PAIR_BUDGET} — see there for the measured ceiling.
 */
function warmUp(layout: Layout, width: number, height: number): void {
  const n = warmupTicks(layout.nodes.length);
  for (let i = 0; i < n; i++) tick(layout, width, height, alphaAt(i));
  layout.warmTicks = n;
}

// --- simulation ------------------------------------------------------------

/** The natural distance between two nodes: the field area divided evenly. */
function spacing(n: number, width: number, height: number): number {
  return Math.sqrt((width * height) / Math.max(n, 1)) * 0.62;
}

/**
 * One tick of a Fruchterman–Reingold-style force simulation: repulsion
 * between every pair, attraction along edges, and a weak pull to the centre
 * so disconnected components don't drift off-canvas.
 *
 * When the layout is grouped, two more forces run: each node is pulled toward
 * its own group's live centroid, and overlapping groups push each other apart.
 * Both are weak against edge attraction on purpose — see {@link COHESION}.
 *
 * `alpha` is the cooling factor — it decays each frame so the graph settles
 * instead of jittering forever.
 *
 * The all-pairs repulsion is O(n²). That is fine here because the caller caps
 * how many nodes reach the canvas (see MAX_RENDERED_NODES in the topology
 * page) and steers larger graphs to `detail=medium|high`, which collapses
 * thousands of assets into tens of nodes. A Barnes–Hut quadtree would lift the
 * cap, but it is a lot of machinery for a view whose readable ceiling is a few
 * hundred nodes anyway.
 */
export function tick(layout: Layout, width: number, height: number, alpha: number): void {
  const { nodes, edges, groups } = layout;
  const k = spacing(nodes.length, width, height);
  // 1 when flat, so the repulsion pass below is arithmetically identical to
  // the ungrouped simulation.
  const xrep = groups.length > 0 ? CROSS_GROUP_REPULSION : 1;

  for (let i = 0; i < nodes.length; i++) {
    const a = nodes[i];
    for (let j = i + 1; j < nodes.length; j++) {
      const b = nodes[j];
      let dx = a.x - b.x;
      let dy = a.y - b.y;
      let d2 = dx * dx + dy * dy;
      if (d2 === 0) {
        // Exactly coincident nodes have no direction to separate along;
        // nudge them deterministically by index so the frame isn't NaN.
        dx = (i % 7) - 3 || 1;
        dy = (j % 7) - 3 || 1;
        d2 = dx * dx + dy * dy;
      }
      const d = Math.sqrt(d2);
      const force = ((k * k) / d2) * (a.gi === b.gi ? 1 : xrep);
      const fx = (dx / d) * force;
      const fy = (dy / d) * force;
      a.vx += fx;
      a.vy += fy;
      b.vx -= fx;
      b.vy -= fy;
    }
  }

  for (const { from, to } of edges) {
    const dx = to.x - from.x;
    const dy = to.y - from.y;
    const d = Math.sqrt(dx * dx + dy * dy) || 1;
    const force = (d * d) / k / 14;
    const fx = (dx / d) * force;
    const fy = (dy / d) * force;
    from.vx += fx;
    from.vy += fy;
    to.vx -= fx;
    to.vy -= fy;
  }

  const cx = width / 2;
  const cy = height / 2;
  if (groups.length > 0) {
    measureGroups(groups);
    separateGroups(groups);
    for (const g of groups) {
      const gx = (cx - g.cx) * GRAVITY_GROUPED;
      const gy = (cy - g.cy) * GRAVITY_GROUPED;
      for (const n of g.members) {
        n.vx += gx + (g.cx - n.x) * COHESION;
        n.vy += gy + (g.cy - n.y) * COHESION;
      }
    }
  } else {
    for (const n of nodes) {
      n.vx += (cx - n.x) * GRAVITY_FLAT;
      n.vy += (cy - n.y) * GRAVITY_FLAT;
    }
  }

  // Clamp the per-frame step. Without it a dense graph's first few frames
  // fling nodes thousands of pixels away and the view never recovers.
  const maxStep = Math.min(MAX_STEP_CEILING, k * MAX_STEP_K);
  for (const n of nodes) {
    if (n.fixed) {
      n.vx = 0;
      n.vy = 0;
      continue;
    }
    const speed = Math.hypot(n.vx, n.vy);
    const scale = speed > maxStep ? maxStep / speed : 1;
    n.x += n.vx * scale * alpha;
    n.y += n.vy * scale * alpha;
    n.vx *= DAMPING;
    n.vy *= DAMPING;
  }
}

/**
 * Refreshes each group's centroid and the radius the separation force works
 * with — how much canvas the cluster is actually using.
 *
 * √2 × the RMS distance to the centroid: for points spread evenly over a disc
 * the RMS distance is R/√2, so this recovers R. RMS rather than the maximum
 * because the maximum is exactly the quantity a cross-group node ruins — one
 * member dragged toward a neighbouring cloud would read as the whole cluster
 * being that wide, and since separating the clusters drags it further still,
 * the two forces would ratchet the picture apart without ever settling. Under
 * RMS a single outlier at distance D over m members adds only D/√m.
 *
 * {@link GROUP_MAX_GROWTH} caps the same failure for the small groups where
 * D/√m is not small enough to be self-limiting.
 */
function measureGroups(groups: LayoutGroup[]): void {
  for (const g of groups) {
    const m = g.members.length;
    if (m === 0) continue;
    let sx = 0;
    let sy = 0;
    for (const n of g.members) {
      sx += n.x;
      sy += n.y;
    }
    g.cx = sx / m;
    g.cy = sy / m;
    let sq = 0;
    for (const n of g.members) {
      const d = Math.hypot(n.x - g.cx, n.y - g.cy) + n.r;
      sq += d * d;
    }
    g.radius = Math.min(Math.sqrt(sq / m) * Math.SQRT2, g.seedR * GROUP_MAX_GROWTH);
  }
}

/** Pushes overlapping clusters apart along the line between their centroids. */
function separateGroups(groups: LayoutGroup[]): void {
  if (groups.length < 2 || groups.length > MAX_SEPARATED_GROUPS) return;
  for (let i = 0; i < groups.length; i++) {
    const a = groups[i];
    for (let j = i + 1; j < groups.length; j++) {
      const b = groups[j];
      let dx = a.cx - b.cx;
      let dy = a.cy - b.cy;
      let d = Math.hypot(dx, dy);
      const want = a.radius + b.radius + GROUP_GAP;
      if (d >= want) continue;
      if (d < 1e-6) {
        // Two clusters exactly on top of each other have no axis to separate
        // along; pick one from the indices so the run stays reproducible.
        dx = (i % 5) - 2 || 1;
        dy = (j % 5) - 2 || 1;
        d = Math.hypot(dx, dy);
      }
      const push = (want - d) * SEPARATION;
      const ux = (dx / d) * push;
      const uy = (dy / d) * push;
      for (const n of a.members) {
        n.vx += ux;
        n.vy += uy;
      }
      for (const n of b.members) {
        n.vx -= ux;
        n.vy -= uy;
      }
    }
  }
}

/** Bounding box of the laid-out nodes, padded, for fit-to-view. */
export function bounds(nodes: LayoutNode[], pad = 60) {
  if (nodes.length === 0) return { minX: 0, minY: 0, maxX: 100, maxY: 100 };
  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const n of nodes) {
    minX = Math.min(minX, n.x - n.r);
    minY = Math.min(minY, n.y - n.r);
    maxX = Math.max(maxX, n.x + n.r);
    maxY = Math.max(maxY, n.y + n.r);
  }
  return { minX: minX - pad, minY: minY - pad, maxX: maxX + pad, maxY: maxY + pad };
}

/**
 * The cluster a node belongs to under `dim`. Mirrors `groupOf` in
 * internal/topology/render.go — including the fallbacks, so the browser's
 * group blobs carry the same labels as an exported Graphviz cluster.
 */
export function groupOf(a: Asset, dim: string): string {
  switch (dim) {
    case 'provider':
      return a.provider;
    case 'account':
      return a.account_id || a.provider;
    case 'region':
      return a.region || '(no region)';
    default:
      return '';
  }
}

/**
 * How far each edge bows off the straight chord between its endpoints,
 * returned parallel to `edges`.
 *
 * The perpendicular is taken in the edge's own from→to frame (see
 * {@link edgeGeometry}), and that frame flips for the opposite direction — so
 * a reciprocal pair separates onto opposite sides of the chord without either
 * edge having to know the other exists. All this index has to handle is
 * *parallel* edges: two kinds joining the same pair the same way, which is
 * common once traffic-flow policy lands on top of a request path.
 */
export function bowOffsets(edges: readonly LayoutEdge[], step = 26): number[] {
  const seen = new Map<string, number>();
  const out = new Array<number>(edges.length);
  for (let i = 0; i < edges.length; i++) {
    const a = edges[i].from.key;
    const b = edges[i].to.key;
    const pair = a < b ? `${a} ${b}` : `${b} ${a}`;
    const n = seen.get(pair) ?? 0;
    seen.set(pair, n + 1);
    out[i] = step * (n + 1);
  }
  return out;
}

export interface EdgeGeometry {
  /** `M … Q …` path data, trimmed at both ends to the node boundary. */
  d: string;
  /** The curve's midpoint, for a label or a hit target. */
  mid: Point;
}

/**
 * A quadratic bezier from `from` to `to`, bowed `bow` units off the chord and
 * clipped to both node circles.
 *
 * Clipping matters more than it sounds: an arrowhead parked at the node's
 * *centre* is buried under the node, so every edge in the diagram looks
 * undirected. `headroom` is extra clearance at the target end for the marker
 * to occupy.
 *
 * The ends are trimmed along the curve's end tangents — a quadratic leaves P0
 * heading straight for the control point and arrives at P1 straight from it,
 * so those two chords *are* the tangents. The exact circle intersection is a
 * quartic root; at the bows drawn here the two answers differ by well under a
 * pixel. Both trims are capped at 40% of the chord so two nodes that end up
 * nearly touching still get a visible stub rather than an inside-out curve.
 */
export function edgeGeometry(
  from: LayoutNode,
  to: LayoutNode,
  bow: number,
  headroom = 0,
): EdgeGeometry | null {
  const dx = to.x - from.x;
  const dy = to.y - from.y;
  const len = Math.hypot(dx, dy);
  if (len < 0.5) return null; // coincident nodes have no direction to draw along

  const cx = (from.x + to.x) / 2 + (-dy / len) * bow;
  const cy = (from.y + to.y) / 2 + (dx / len) * bow;
  const ctrl = { x: cx, y: cy };

  const s = along(from, ctrl, Math.min(from.r, len * 0.4));
  const e = along(to, ctrl, Math.min(to.r + headroom, len * 0.4));

  return {
    d: `M${r2(s.x)} ${r2(s.y)}Q${r2(cx)} ${r2(cy)} ${r2(e.x)} ${r2(e.y)}`,
    // B(0.5) of a quadratic — a quarter of each endpoint plus half the control.
    mid: { x: 0.25 * s.x + 0.5 * cx + 0.25 * e.x, y: 0.25 * s.y + 0.5 * cy + 0.25 * e.y },
  };
}

function along(from: Point, toward: Point, dist: number): Point {
  const dx = toward.x - from.x;
  const dy = toward.y - from.y;
  const d = Math.hypot(dx, dy) || 1;
  return { x: from.x + (dx / d) * dist, y: from.y + (dy / d) * dist };
}

/** Path strings are rebuilt on every settle frame and diffed by React as
 *  strings; two decimals is sub-pixel at any zoom the view allows and keeps
 *  seventeen digits of float noise out of each coordinate. */
function r2(n: number): number {
  return Math.round(n * 100) / 100;
}
