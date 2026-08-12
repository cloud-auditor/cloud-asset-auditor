/**
 * Convex hulls for the topology view's group blobs.
 *
 * Hand-rolled for the same reason as everything else in lib/: the bundle is
 * embedded in the Go binary, and this is forty lines of textbook geometry
 * behind a dependency that would have to be pinned, audited, and rebuilt.
 *
 * Nothing here knows about React, SVG viewports, or assets — it takes points
 * and returns points (plus one path string), so it can be reasoned about
 * without a browser.
 */

export interface Point {
  x: number;
  y: number;
}

/** A node as the hull sees it: the blob must enclose the drawn circle, not
 *  its centre, or nodes on the boundary hang half outside their own group. */
export interface Disc extends Point {
  r: number;
}

/** Cross product of OA × OB. Sign tells which way O→A→B turns. */
function cross(o: Point, a: Point, b: Point): number {
  return (a.x - o.x) * (b.y - o.y) - (a.y - o.y) * (b.x - o.x);
}

/**
 * Andrew's monotone chain, O(n log n).
 *
 * The `<= 0` pop drops collinear points as well as reflex ones, which matters
 * here: {@link discHull} feeds in regularly-sampled circle points, and a hull
 * that kept every collinear sample would carry hundreds of vertices for a
 * shape with six corners — and {@link roundedPath} would then round each of
 * them by zero and emit a path ten times longer than it needs to be.
 *
 * Fewer than three input points cannot bound an area; they come back
 * unchanged so the caller can decide (and every caller here skips them).
 */
export function convexHull(points: readonly Point[]): Point[] {
  if (points.length < 3) return points.slice();

  const sorted = points.slice().sort((a, b) => a.x - b.x || a.y - b.y);

  const chain = (src: readonly Point[]): Point[] => {
    const out: Point[] = [];
    for (const p of src) {
      while (out.length >= 2 && cross(out[out.length - 2], out[out.length - 1], p) <= 0) out.pop();
      out.push(p);
    }
    // The chain's last point is the other chain's first — dropping it here is
    // what makes the concatenation below a closed ring with no duplicates.
    out.pop();
    return out;
  };

  return chain(sorted).concat(chain(sorted.slice().reverse()));
}

/**
 * Hull of a set of padded discs.
 *
 * Sampling `segments` points around each circle and hulling the lot, rather
 * than hulling the centres and offsetting the result outward: a polygon
 * offset needs mitred edge normals, and a mitre at a sharp vertex shoots off
 * to infinity — which on a force-directed layout (which produces plenty of
 * sharp vertices) means the occasional group blob with a spike through the
 * rest of the diagram. Sampling cannot do that, and 10 points per node is
 * nothing next to the O(n²) force pass that placed them.
 *
 * The sampling has one cost, which {@link simplify} pays off: 10 points per
 * disc means the hull of a 50-node group arrives with dozens of vertices a
 * few pixels apart, and {@link roundedPath} clamps each corner's radius to
 * half the shorter adjacent edge — so a blob made of very short edges rounds
 * by two or three pixels and draws as the faceted polygon it is.
 */
export function discHull(discs: readonly Disc[], pad: number, segments = 10): Point[] {
  const pts: Point[] = [];
  for (const d of discs) {
    const r = d.r + pad;
    for (let i = 0; i < segments; i++) {
      const a = (i / segments) * Math.PI * 2;
      pts.push({ x: d.x + Math.cos(a) * r, y: d.y + Math.sin(a) * r });
    }
  }
  return simplify(convexHull(pts), pad * SIMPLIFY_FRACTION);
}

/**
 * How much of the padding {@link simplify} may spend — the hard bound on how
 * far the ring can retreat toward the nodes it encloses.
 */
const SIMPLIFY_FRACTION = 0.3;

/**
 * Drops vertices that sit within `epsilon` of the chord that would replace
 * them, so the hull keeps its shape but loses the near-collinear clutter that
 * circle sampling produces.
 *
 * Dropping a vertex from a convex polygon only ever moves the boundary
 * *inward*, so the bound on that retreat is the whole safety argument: at
 * `epsilon` = 30% of the padding, a member disc keeps 70% of its clearance and
 * can never be left outside its own blob.
 *
 * Earning that bound is why every vertex dropped since the last kept one is
 * re-checked against each new chord. Testing a candidate only against its
 * immediate neighbours is the obvious version and it is wrong: across a run of
 * drops each test measures a different chord, the errors compound, and a
 * sampled circular arc — whose points are all locally near-collinear —
 * decimates to a single chord straight across it. That version passed at a
 * 34px pad and cut nodes clean outside their hull at 60px, which is the kind
 * of bug that waits for someone to retune a constant in another file.
 */
function simplify(hull: readonly Point[], epsilon: number): Point[] {
  const n = hull.length;
  if (n < 5 || epsilon <= 0) return hull.slice();

  const out: Point[] = [];
  let dropped: Point[] = [];
  for (let i = 0; i < n; i++) {
    // The first vertex is always kept, so the ring closes on a kept vertex and
    // every chord below runs between two of them.
    if (i > 0) {
      const prev = out[out.length - 1];
      const next = hull[(i + 1) % n];
      let fits = segDistance(hull[i], prev, next) < epsilon;
      if (fits) {
        for (const p of dropped) {
          if (segDistance(p, prev, next) >= epsilon) {
            fits = false;
            break;
          }
        }
      }
      if (fits) {
        dropped.push(hull[i]);
        continue;
      }
    }
    out.push(hull[i]);
    dropped = [];
  }
  // Cutting below a triangle would leave nothing to draw; the unsimplified
  // ring is a better answer than an empty one.
  return out.length >= 3 ? out : hull.slice();
}

/** Perpendicular distance from `p` to the infinite line ab (the segment ends
 *  are hull vertices either side of `p`, so the foot always lands between). */
function segDistance(p: Point, a: Point, b: Point): number {
  const dx = b.x - a.x;
  const dy = b.y - a.y;
  const len = Math.hypot(dx, dy);
  if (len < 1e-9) return Math.hypot(p.x - a.x, p.y - a.y);
  return Math.abs(dx * (a.y - p.y) - dy * (a.x - p.x)) / len;
}

export interface GroupHull {
  /** The hull ring. Empty when there was nothing to bound. */
  points: Point[];
  /** The padding that survived the foreign-point test. */
  pad: number;
  /**
   * Non-members still inside the ring. **Zero is not guaranteed** — see
   * {@link groupHull}. A caller that cares can use it to soften the fill, or
   * to say nothing at all rather than say something false.
   */
  enclosed: number;
}

/** Padding floor for {@link groupHull}: below this the ring cuts into the node
 *  circles it is supposed to contain, and the blob reads as a mistake rather
 *  than a tight fit. */
const MIN_PAD = 9;

/**
 * The hull to draw around one group, given every node on the canvas.
 *
 * A convex hull around a cluster can enclose a node that is not in it, and on
 * a force layout that is not rare: a group with one member dragged out toward
 * another cloud has a long thin hull, and everything in the wedge behind that
 * arm falls inside. Drawn naively the blob claims assets that belong to a
 * different provider, which is a factual error in a diagram whose entire job
 * is saying what belongs where.
 *
 * A good half of those captures are the *padding*, not the geometry: on the
 * demo inventory at group-by provider, a fixed 34px ring catches ten
 * non-members and five of them sit within 20px of the boundary — inside only
 * because the ring was inflated around them. So this retries with less padding
 * and keeps the roomiest ring that catches nobody, which halves that to five.
 * Group-by region goes from two captures to none.
 *
 * The rest are not fixable this way and the function does not pretend
 * otherwise: a point inside the convex closure of the members is inside
 * *every* convex ring containing them, at any padding. Excluding it needs a
 * concave hull, and a concave blob around a force layout is the worse lie — it
 * grows notches and inlets that read as structure that is not there. So the
 * residue comes back in {@link GroupHull.enclosed} for the caller to judge
 * rather than being quietly drawn as fact.
 *
 * `all` may include the members themselves — they are skipped by identity, so
 * the caller does not have to build the complement set.
 */
export function groupHull(
  members: readonly Disc[],
  all: readonly Point[],
  pad: number,
  segments = 10,
): GroupHull {
  if (members.length === 0) return { points: [], pad, enclosed: 0 };

  const own = new Set<Point>(members);
  const foreign = all.filter((p) => !own.has(p));

  // Three tries, not a search: each one is a hull pass over every sampled
  // point, and the difference between a 34px ring and a 21px one is not worth
  // a binary search's worth of them. Strictly-fewer keeps the roomiest ring
  // when two paddings capture the same number, so nothing shrinks for nothing.
  let best: GroupHull = { points: [], pad, enclosed: Infinity };
  for (const p of [pad, (pad + MIN_PAD) / 2, MIN_PAD]) {
    const points = discHull(members, p, segments);
    const enclosed = countInside(points, foreign);
    if (enclosed < best.enclosed) best = { points, pad: p, enclosed };
    if (enclosed === 0) break;
  }
  return best;
}

/** How many of `points` fall inside the convex ring `hull`. */
function countInside(hull: readonly Point[], points: readonly Point[]): number {
  if (hull.length < 3) return 0;

  let minX = Infinity;
  let minY = Infinity;
  let maxX = -Infinity;
  let maxY = -Infinity;
  for (const p of hull) {
    if (p.x < minX) minX = p.x;
    if (p.y < minY) minY = p.y;
    if (p.x > maxX) maxX = p.x;
    if (p.y > maxY) maxY = p.y;
  }

  let n = 0;
  for (const p of points) {
    // The box test is what keeps this affordable: it is four comparisons
    // against a per-vertex walk, and on a spread-out graph almost every node
    // is outside almost every group's box.
    if (p.x < minX || p.x > maxX || p.y < minY || p.y > maxY) continue;
    if (insideConvex(hull, p)) n++;
  }
  return n;
}

/**
 * Point-in-convex-polygon by sign consistency.
 *
 * The usual crossing-number test always walks every edge; this one bails at
 * the first edge the point is outside of, which is the common case here — and
 * it is only valid because {@link convexHull} guarantees convexity and a
 * consistent winding. The winding itself is not assumed: the first non-zero
 * cross product defines the sign the rest must match.
 */
function insideConvex(hull: readonly Point[], p: Point): boolean {
  let sign = 0;
  for (let i = 0; i < hull.length; i++) {
    const a = hull[i];
    const b = hull[(i + 1) % hull.length];
    const c = (b.x - a.x) * (p.y - a.y) - (b.y - a.y) * (p.x - a.x);
    if (c === 0) continue; // exactly on an edge: not a counterexample either way
    const s = c > 0 ? 1 : -1;
    if (sign === 0) sign = s;
    else if (s !== sign) return false;
  }
  return true;
}

/**
 * A closed SVG path around `hull` with its corners rounded.
 *
 * Each corner's radius is clamped to half of the shorter adjacent edge, so a
 * narrow spike rounds by however much fits instead of overshooting into a
 * self-crossing loop. Returns '' for a degenerate hull.
 */
export function roundedPath(hull: readonly Point[], radius: number): string {
  const n = hull.length;
  if (n < 3) return '';

  const parts: string[] = [];
  for (let i = 0; i < n; i++) {
    const v = hull[i];
    const prev = hull[(i - 1 + n) % n];
    const next = hull[(i + 1) % n];

    const dPrev = Math.hypot(v.x - prev.x, v.y - prev.y) || 1;
    const dNext = Math.hypot(next.x - v.x, next.y - v.y) || 1;
    const r = Math.min(radius, dPrev / 2, dNext / 2);

    const entry = {
      x: v.x + ((prev.x - v.x) / dPrev) * r,
      y: v.y + ((prev.y - v.y) / dPrev) * r,
    };
    const exit = {
      x: v.x + ((next.x - v.x) / dNext) * r,
      y: v.y + ((next.y - v.y) / dNext) * r,
    };

    parts.push(
      `${i === 0 ? 'M' : 'L'}${round(entry.x)} ${round(entry.y)}` +
        `Q${round(v.x)} ${round(v.y)} ${round(exit.x)} ${round(exit.y)}`,
    );
  }
  // Z draws the final edge back to the first corner's entry point, which is
  // exactly the segment from the last vertex to the first.
  return `${parts.join('')}Z`;
}

/** Two decimals is under a tenth of a pixel at any zoom the view allows, and
 *  it keeps these path strings — which React diffs on every settle frame —
 *  from carrying seventeen digits of float noise per coordinate. */
function round(n: number): number {
  return Math.round(n * 100) / 100;
}
