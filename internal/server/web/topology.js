// Cloud Asset Auditor — Topology tab. A hand-rolled force-directed graph
// over /api/v1/topology, rendered to SVG. Vanilla JS only: the project rule
// is "no third-party JS, no build step", so the physics, pan/zoom, and
// rendering below are all bespoke (and deliberately small).
//
// Shape of the module:
//
//   buildGraph()        fetch the topology (abortable — a build runs a full
//                       audit server-side and can take minutes)
//   loadGraph(data)     index nodes/edges, deterministic spiral placement,
//                       (re)build the SVG scene
//   frame()/tickSim()   rAF physics loop; cools from alpha=1 to ALPHA_MIN
//                       then freezes itself; drags/re-layout reheat it
//   sync*()             write positions/visibility into the live SVG
//
// The only cross-file contract is window.auditorShared (defined by app.js):
// the provider checkboxes on the Assets tab decide which providers a build
// targets, exactly like "Run audit".
//
// With location.hash === "#demo" the Build button loads DEMO_GRAPH (bottom
// of this file) instead of calling the API, so the whole feature is
// testable with zero cloud credentials.

(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);

  // ---------- palette ----------
  // Node fill by provider; edge stroke by kind. Single source of truth:
  // the arrowhead markers and the legend are both generated from these.

  const PROVIDER_COLORS = {
    cloudflare: "#f6821f",
    oci:        "#c74634",
    kubernetes: "#326ce5",
  };
  const PROVIDER_FALLBACK = "#6b7280";

  const EDGE_COLORS = {
    "dns":             "#8b5cf6",
    "waf":             "#f59e0b",
    "lb-backend":      "#10b981",
    "gateway-route":   "#3b82f6",
    "service-backend": "#14b8a6",
  };
  const EDGE_FALLBACK = "#9ca3af";

  // ---------- physics constants ----------
  // Classic force-directed recipe: every node pair repels (Coulomb, k/d²),
  // every edge is a spring pulling toward REST_LENGTH, and weak gravity
  // keeps disconnected components from drifting off-screen. All forces are
  // scaled by a cooling "alpha" so the layout settles instead of jittering
  // forever.

  const REPULSION   = 3600;  // Coulomb constant; larger = airier layout
  const MAX_FORCE   = 12;    // per-pair force cap; stops overlap blow-ups
  const SPRING_K    = 0.05;  // edge stiffness; small, so repulsion dominates shape
  const REST_LENGTH = 100;   // px an edge "wants" to be, at scale 1
  const GRAVITY     = 0.02;  // pull toward (0,0); just enough to keep orphans nearby
  const DAMPING     = 0.85;  // velocity kept per tick; <1 or the sim oscillates
  const ALPHA_DECAY = 0.985; // 1.0 → ~0.02 in ~260 ticks (≈4 s at 60 fps)
  const ALPHA_MIN   = 0.02;  // below this the layout is "settled": freeze
  const DRAG_ALPHA  = 0.3;   // mild reheat while dragging, so neighbours adjust

  const EXACT_PAIR_LIMIT = 1500; // beyond this, sample repulsion pairs per tick
  const SAMPLE_PARTNERS  = 24;   // partners per node per tick in sampled mode

  // ---------- view constants ----------

  const SCALE_MIN = 0.08, SCALE_MAX = 8;
  const FIT_MAX_SCALE   = 1.25; // never zoom-to-fit closer than this (tiny graphs)
  const LABEL_MIN_SCALE = 0.6;  // hide labels when zoomed out past this
  const LABEL_MAX_CHARS = 24;
  const CLICK_SLOP      = 4;    // px of pointer travel that still counts as a click

  // ---------- state ----------

  const state = {
    nodes: [],         // [{asset, key, idx, x, y, vx, vy, r, el, labelEl}]
    byKey: new Map(),  // refKey → node
    links: [],         // [{edge, a, b, el}] — only edges with both endpoints resolved
    rawEdges: [],      // every Edge from the response (panel lists unresolved peers too)
    alpha: 0,
    tick: 0,           // seeds the sampled-repulsion LCG
    raf: 0,            // requestAnimationFrame handle; 0 when frozen
    view: { x: 0, y: 0, k: 1 },
    labelsOn: true,    // cached bulk label visibility, to skip O(n) churn per wheel event
    hovered: null,
    selected: null,
    drag: null,        // {mode:"pan"|"node", ...} while a pointer is down
    autoFit: false,    // zoom-to-fit once when the sim freezes (unless the user took the camera)
    abort: null,       // AbortController while a build is in flight
    timer: 0,          // elapsed-seconds interval while building
    svg: null, viewport: null, edgesG: null, nodesG: null, labelsG: null,
    ring: null,        // selection-highlight circle
  };

  // ---------- bootstrap ----------

  function init() {
    state.svg = $("#topo-svg");
    buildSvgScaffold();
    buildLegend();

    $("#topo-build-btn").addEventListener("click", buildGraph);
    $("#topo-build-assets-btn").addEventListener("click", buildFromAssets);
    $("#topo-cancel-btn").addEventListener("click", () => state.abort && state.abort.abort());
    $("#topo-fit-btn").addEventListener("click", zoomToFit);
    $("#topo-relayout-btn").addEventListener("click", relayout);
    $("#topo-export-json").addEventListener("click",       () => exportTopo("json"));
    $("#topo-export-dot").addEventListener("click",        () => exportTopo("dot"));
    $("#topo-export-mermaid").addEventListener("click",    () => exportTopo("mermaid"));
    $("#topo-export-excalidraw").addEventListener("click", () => exportTopo("excalidraw"));

    state.svg.addEventListener("wheel", onWheel, { passive: false });
    state.svg.addEventListener("pointerdown", onPointerDown);
    state.svg.addEventListener("pointermove", onPointerMove);
    state.svg.addEventListener("pointerup", onPointerUp);
    state.svg.addEventListener("pointercancel", onPointerUp);
    state.svg.addEventListener("dblclick", onDblClick);
    document.addEventListener("keydown", onKeyDown);

    updateBuildLabel();
    window.addEventListener("hashchange", updateBuildLabel);
  }

  document.addEventListener("DOMContentLoaded", init);

  const demoMode = () => location.hash === "#demo";

  function updateBuildLabel() {
    $("#topo-build-btn").textContent = demoMode() ? "Load demo" : "Build graph";
  }

  // ---------- svg scaffold ----------

  // One <defs> block (an arrowhead marker per edge kind, plus a fallback)
  // and a single pan/zoom <g> holding edges-under-nodes-under-labels
  // sub-groups. Built once; loadGraph only ever repopulates the sub-groups.
  function buildSvgScaffold() {
    const defs = svgEl("defs");
    for (const kind of Object.keys(EDGE_COLORS).concat("default")) {
      const m = svgEl("marker", {
        id: "topo-arrow-" + kind,
        viewBox: "0 0 8 8", refX: 7, refY: 4,
        markerWidth: 7, markerHeight: 7, orient: "auto",
      });
      m.appendChild(svgEl("path", { d: "M0,0 L8,4 L0,8 Z", fill: EDGE_COLORS[kind] || EDGE_FALLBACK }));
      defs.appendChild(m);
    }
    state.svg.appendChild(defs);

    state.viewport = svgEl("g");
    state.edgesG  = svgEl("g");
    state.nodesG  = svgEl("g");
    state.labelsG = svgEl("g");
    state.ring = svgEl("circle", { class: "topo-sel-ring", display: "none" });
    state.viewport.append(state.edgesG, state.nodesG, state.ring, state.labelsG);
    state.svg.appendChild(state.viewport);
  }

  function svgEl(tag, attrs = {}) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    return node;
  }

  // Tiny hyperscript helper for the legend and details panel:
  // el("dd", {class: "x"}, "text", childNode).
  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else node.setAttribute(k, v);
    }
    node.append(...children);
    return node;
  }

  // ---------- legend ----------

  function buildLegend() {
    const box = $("#topo-legend");
    box.appendChild(el("strong", {}, "Providers"));
    for (const [name, color] of Object.entries({ ...PROVIDER_COLORS, other: PROVIDER_FALLBACK })) {
      box.appendChild(el("div", { class: "legend-row" }, dotSwatch(color), name));
    }
    box.appendChild(el("strong", {}, "Edge kinds"));
    for (const [kind, color] of Object.entries({ ...EDGE_COLORS, other: EDGE_FALLBACK })) {
      box.appendChild(el("div", { class: "legend-row" }, lineSwatch(color, false), kind));
    }
    box.appendChild(el("strong", {}, "Confidence"));
    box.appendChild(el("div", { class: "legend-row" }, lineSwatch("#57606a", false), "exact"));
    box.appendChild(el("div", { class: "legend-row" }, lineSwatch("#57606a", true), "heuristic"));
  }

  function dotSwatch(color) {
    const d = el("span", { class: "dot" });
    d.style.background = color;
    return d;
  }

  // A 22×6 inline SVG line, so dashed-vs-solid renders in the legend
  // exactly as it does on the canvas.
  function lineSwatch(color, dashed) {
    const svg = svgEl("svg", { width: 22, height: 6 });
    const ln = svgEl("line", { x1: 0, y1: 3, x2: 22, y2: 3, stroke: color, "stroke-width": 2 });
    if (dashed) ln.setAttribute("stroke-dasharray", "4 3");
    svg.appendChild(ln);
    return svg;
  }

  // ---------- build ----------

  // Query params shared by build and export. Mirrors the Assets tab: only
  // send providers= when a strict subset is selected (server default = all).
  function topoParams() {
    const params = new URLSearchParams();
    const all = window.auditorShared.allProviders();
    const sel = window.auditorShared.selectedProviders();
    if (sel.length > 0 && sel.length < all.length) params.set("providers", sel.join(","));
    if ($("#topo-orphans").checked) params.set("include-orphans", "true");
    return params;
  }

  async function buildGraph() {
    if (state.abort) return; // already building

    clearTopoErrors();
    if (demoMode()) {
      loadGraph(DEMO_GRAPH);
      setTopoStatus(`Demo: ${state.nodes.length} nodes, ${state.rawEdges.length} edges.`);
      return;
    }

    const params = topoParams();
    params.set("format", "json");

    state.abort = new AbortController();
    $("#topo-build-btn").hidden = true;
    $("#topo-cancel-btn").hidden = false;

    // The endpoint runs a full audit synchronously before it can answer, so
    // show a live elapsed counter instead of an indeterminate spinner.
    const startedAt = Date.now();
    const tickStatus = () =>
      setTopoStatus(`Building… ${Math.floor((Date.now() - startedAt) / 1000)}s (runs a full audit server-side)`);
    tickStatus();
    state.timer = setInterval(tickStatus, 1000);

    try {
      const r = await fetch("/api/v1/topology?" + params.toString(), { signal: state.abort.signal });
      if (!r.ok) throw new Error(`topology request failed: HTTP ${r.status}`);
      const j = await r.json();
      (j.init_errors || []).forEach((m) => addTopoError("init: " + m));
      (j.errors || []).forEach(addTopoError);
      loadGraph(j);
      const elapsed = ((Date.now() - startedAt) / 1000).toFixed(1);
      setTopoStatus(`${state.nodes.length} nodes, ${state.rawEdges.length} edges in ${elapsed}s.`);
    } catch (e) {
      if (e.name === "AbortError") {
        setTopoStatus("Cancelled.");
      } else {
        addTopoError(e.message);
        setTopoStatus("Build failed.");
      }
    } finally {
      clearInterval(state.timer);
      state.abort = null;
      $("#topo-build-btn").hidden = false;
      $("#topo-cancel-btn").hidden = true;
    }
  }

  // buildFromAssets POSTs the Assets tab's in-memory rows to the server's
  // graph engine — instant, no second audit. The SSE stream doesn't carry
  // Raw payloads, so resolvers that parse them (Ingress/HTTPRoute → Service)
  // can't fire on this path; "Build graph" runs a raw-bearing audit instead.
  async function buildFromAssets() {
    if (state.abort) return; // a fresh-audit build is in flight

    clearTopoErrors();
    if (demoMode()) {
      loadGraph(DEMO_GRAPH);
      setTopoStatus(`Demo: ${state.nodes.length} nodes, ${state.rawEdges.length} edges.`);
      return;
    }

    const assets = window.auditorShared.assets();
    if (assets.length === 0) {
      setTopoStatus("No streamed assets yet — run an audit on the Assets tab first.");
      return;
    }

    const params = new URLSearchParams();
    if ($("#topo-orphans").checked) params.set("include-orphans", "true");
    params.set("format", "json");

    state.abort = new AbortController();
    setTopoStatus(`Building from ${assets.length} streamed assets…`);
    try {
      const r = await fetch("/api/v1/topology?" + params.toString(), {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ assets }),
        signal: state.abort.signal,
      });
      if (!r.ok) throw new Error(`topology request failed: HTTP ${r.status}`);
      loadGraph(await r.json());
      setTopoStatus(
        `${state.nodes.length} nodes, ${state.rawEdges.length} edges from streamed assets ` +
        `(no raw payloads on this path — use Build graph for Ingress→Service edges).`);
    } catch (e) {
      if (e.name === "AbortError") {
        setTopoStatus("Cancelled.");
      } else {
        addTopoError(e.message);
        setTopoStatus("Build failed.");
      }
    } finally {
      state.abort = null;
    }
  }

  function exportTopo(format) {
    const params = topoParams();
    params.set("format", format);
    // Non-JSON formats come back with Content-Disposition: attachment, so
    // plain navigation is the whole download story. Note the server re-runs
    // the audit for each export — same cost as a build.
    window.location.href = "/api/v1/topology?" + params.toString();
  }

  // ---------- graph loading & layout ----------

  // Nodes are identified by provider/type/id — the same tuple Edge.From and
  // Edge.To carry — so edges resolve to node objects with one Map lookup.
  function refKey(ref) {
    return ref.provider + "\u0000" + ref.type + "\u0000" + ref.id;
  }

  function loadGraph(data) {
    selectNode(null);
    state.hovered = null;

    state.nodes = (data.nodes || []).map((asset, idx) => ({
      asset, idx,
      key: refKey(asset),
      x: 0, y: 0, vx: 0, vy: 0,
      r: 7, el: null, labelEl: null,
    }));
    state.byKey = new Map(state.nodes.map((nd) => [nd.key, nd]));

    // Physics and drawing only consider edges with both endpoints present
    // (and no self-loops); the details panel still lists the rest.
    state.rawEdges = data.edges || [];
    state.links = [];
    const degree = new Map();
    for (const edge of state.rawEdges) {
      const a = state.byKey.get(refKey(edge.from));
      const b = state.byKey.get(refKey(edge.to));
      if (!a || !b || a === b) continue;
      state.links.push({ edge, a, b, el: null });
      degree.set(a, (degree.get(a) || 0) + 1);
      degree.set(b, (degree.get(b) || 0) + 1);
    }
    for (const nd of state.nodes) nd.r = 7 + Math.min(degree.get(nd) || 0, 8);

    const empty = $("#topo-empty");
    empty.hidden = state.nodes.length > 0;
    if (state.nodes.length === 0) {
      empty.textContent = "No connected assets found — try include-orphans.";
    }

    placeNodes();
    buildScene();
    zoomToFit();
    state.autoFit = true; // re-fit once settled — the layout outgrows the spiral
    state.alpha = 0;
    startSim(1);
  }

  // Deterministic initial placement: a golden-angle (sunflower) spiral by
  // node index. Same input order → same start → same settled layout, which
  // is why nothing here (or in the sampled repulsion below) touches
  // Math.random.
  const GOLDEN_ANGLE = Math.PI * (3 - Math.sqrt(5)); // ≈ 2.39996 rad

  function placeNodes() {
    const spacing = 24; // radial step ≈ one node diameter + breathing room
    state.nodes.forEach((nd, i) => {
      const r = spacing * Math.sqrt(i + 0.5);
      nd.x = r * Math.cos(i * GOLDEN_ANGLE);
      nd.y = r * Math.sin(i * GOLDEN_ANGLE);
      nd.vx = 0;
      nd.vy = 0;
    });
  }

  function relayout() {
    if (!state.nodes.length) return;
    placeNodes();
    syncPositions();
    zoomToFit();
    state.autoFit = true;
    state.alpha = 0;
    startSim(1);
  }

  // (Re)build every SVG element for the current nodes/links. Positions are
  // written separately by syncPositions, so per-frame work is attribute
  // updates only — never DOM churn.
  function buildScene() {
    state.edgesG.replaceChildren();
    state.nodesG.replaceChildren();
    state.labelsG.replaceChildren();

    for (const link of state.links) {
      const kind = link.edge.kind;
      const known = Object.prototype.hasOwnProperty.call(EDGE_COLORS, kind);
      link.el = svgEl("line", {
        stroke: known ? EDGE_COLORS[kind] : EDGE_FALLBACK,
        "stroke-width": 1.5,
        "marker-end": `url(#topo-arrow-${known ? kind : "default"})`,
      });
      if (link.edge.confidence === "heuristic") link.el.setAttribute("stroke-dasharray", "6 4");
      state.edgesG.appendChild(link.el);
    }

    for (const nd of state.nodes) {
      const c = svgEl("circle", {
        class: "topo-node",
        r: nd.r,
        fill: PROVIDER_COLORS[nd.asset.provider] || PROVIDER_FALLBACK,
      });
      const tip = svgEl("title");
      tip.textContent = `${nd.asset.name || nd.asset.id} (${nd.asset.type})`;
      c.appendChild(tip);
      c.__node = nd; // back-reference for the shared pointer handlers
      c.addEventListener("pointerenter", () => setHovered(nd));
      c.addEventListener("pointerleave", () => setHovered(null));
      nd.el = c;
      state.nodesG.appendChild(c);

      const label = svgEl("text", { class: "topo-label", "text-anchor": "middle" });
      label.textContent = truncate(nd.asset.name || nd.asset.id, LABEL_MAX_CHARS);
      nd.labelEl = label;
      state.labelsG.appendChild(label);
    }

    state.ring.setAttribute("display", "none");
    syncLabelVisibility(true);
    syncPositions();
  }

  // ---------- simulation ----------

  function startSim(alpha) {
    state.alpha = Math.max(state.alpha, alpha);
    if (!state.raf && state.nodes.length) state.raf = requestAnimationFrame(frame);
  }

  function frame() {
    state.raf = 0;
    tickSim();
    syncPositions();
    const dragging = state.drag && state.drag.mode === "node";
    if (state.alpha >= ALPHA_MIN || dragging) {
      state.raf = requestAnimationFrame(frame); // keep cooling (or following the drag)
      return;
    }
    // Settled. The cooled layout is larger than the spiral the initial fit
    // saw, so fit once more — but only if the user hasn't panned, zoomed,
    // or dragged in the meantime (don't yank the camera out of their hands).
    if (state.autoFit) {
      state.autoFit = false;
      zoomToFit();
    }
  }

  function tickSim() {
    const alpha = state.alpha;
    state.tick++;

    applyRepulsion(alpha);

    // Springs: pull each edge's endpoints toward REST_LENGTH apart.
    for (const { a, b } of state.links) {
      const dx = b.x - a.x, dy = b.y - a.y;
      const d = Math.hypot(dx, dy) || 1e-3;
      const f = SPRING_K * (d - REST_LENGTH) * alpha;
      const fx = (dx / d) * f, fy = (dy / d) * f;
      a.vx += fx; a.vy += fy;
      b.vx -= fx; b.vy -= fy;
    }

    const dragged = state.drag && state.drag.mode === "node" ? state.drag.node : null;
    for (const nd of state.nodes) {
      if (nd === dragged) { nd.vx = 0; nd.vy = 0; continue; } // pinned under the cursor
      // Weak gravity toward the origin keeps disconnected clusters on screen.
      nd.vx -= nd.x * GRAVITY * alpha;
      nd.vy -= nd.y * GRAVITY * alpha;
      nd.vx *= DAMPING;
      nd.vy *= DAMPING;
      nd.x += nd.vx;
      nd.y += nd.vy;
    }

    state.alpha *= ALPHA_DECAY;
  }

  // Pairwise Coulomb repulsion. Exact O(n²) up to EXACT_PAIR_LIMIT nodes;
  // beyond that each node repels only SAMPLE_PARTNERS pseudo-random
  // partners per tick, scaled up to approximate the full sum. The partner
  // sequence comes from a tick-seeded LCG, not Math.random — deterministic,
  // so the same data still lands in the same layout, while different
  // partners each tick average out to the true field.
  function applyRepulsion(alpha) {
    const nodes = state.nodes;
    const n = nodes.length;
    if (n <= EXACT_PAIR_LIMIT) {
      for (let i = 0; i < n; i++) {
        for (let j = i + 1; j < n; j++) repel(nodes[i], nodes[j], alpha, 1, false);
      }
      return;
    }
    let seed = (state.tick * 2654435761) >>> 0;
    const next = () => (seed = (seed * 1664525 + 1013904223) >>> 0) / 4294967296;
    const scale = (n - 1) / SAMPLE_PARTNERS; // sampled sum ≈ exact sum
    for (let i = 0; i < n; i++) {
      for (let s = 0; s < SAMPLE_PARTNERS; s++) {
        const j = (next() * n) | 0;
        if (j !== i) repel(nodes[i], nodes[j], alpha, scale, true);
      }
    }
  }

  function repel(a, b, alpha, scale, oneSided) {
    let dx = a.x - b.x, dy = a.y - b.y;
    let d2 = dx * dx + dy * dy;
    if (d2 < 1e-4) {
      // Coincident nodes have no direction to push along; nudge them apart
      // by index so they separate the same way every run.
      dx = 0.1 * (a.idx < b.idx ? -1 : 1);
      dy = 0.05;
      d2 = dx * dx + dy * dy;
    }
    const d = Math.sqrt(d2);
    const f = Math.min((REPULSION * scale * alpha) / d2, MAX_FORCE);
    const fx = (dx / d) * f, fy = (dy / d) * f;
    a.vx += fx; a.vy += fy;
    if (!oneSided) { b.vx -= fx; b.vy -= fy; }
  }

  // ---------- position → SVG ----------

  function syncPositions() {
    for (const link of state.links) {
      const { a, b } = link;
      const dx = b.x - a.x, dy = b.y - a.y;
      const d = Math.hypot(dx, dy);
      let x1 = a.x, y1 = a.y, x2 = b.x, y2 = b.y;
      // Pull each endpoint back to the node's rim so the arrowhead lands on
      // the circle's edge instead of underneath it.
      if (d > a.r + b.r + 4) {
        const ux = dx / d, uy = dy / d;
        x1 += ux * a.r;       y1 += uy * a.r;
        x2 -= ux * (b.r + 3); y2 -= uy * (b.r + 3); // +3 leaves room for the marker tip
      }
      link.el.setAttribute("x1", x1); link.el.setAttribute("y1", y1);
      link.el.setAttribute("x2", x2); link.el.setAttribute("y2", y2);
    }
    for (const nd of state.nodes) {
      nd.el.setAttribute("cx", nd.x);
      nd.el.setAttribute("cy", nd.y);
      nd.labelEl.setAttribute("x", nd.x);
      nd.labelEl.setAttribute("y", nd.y + nd.r + 12);
    }
    if (state.selected) {
      state.ring.setAttribute("cx", state.selected.x);
      state.ring.setAttribute("cy", state.selected.y);
    }
  }

  // Labels are hidden wholesale below LABEL_MIN_SCALE — except the hovered
  // and selected nodes, which always show. The bulk O(n) pass only runs
  // when the zoom crosses the threshold (state.labelsOn caches the last
  // answer); hover/select changes touch just their own label.
  function syncLabelVisibility(force = false) {
    const on = state.view.k >= LABEL_MIN_SCALE;
    if (!force && on === state.labelsOn) return;
    state.labelsOn = on;
    for (const nd of state.nodes) syncOneLabel(nd);
  }

  function syncOneLabel(nd) {
    const show = state.labelsOn || nd === state.hovered || nd === state.selected;
    if (show) nd.labelEl.removeAttribute("display");
    else nd.labelEl.setAttribute("display", "none");
  }

  function setHovered(nd) {
    if (state.hovered === nd) return;
    const prev = state.hovered;
    state.hovered = nd;
    if (prev) {
      prev.el.classList.remove("hover");
      syncOneLabel(prev);
    }
    if (nd) {
      nd.el.classList.add("hover");
      syncOneLabel(nd);
    }
  }

  // ---------- pan / zoom ----------

  function updateViewTransform() {
    const { x, y, k } = state.view;
    state.viewport.setAttribute("transform", `translate(${x},${y}) scale(${k})`);
    syncLabelVisibility();
  }

  function screenToWorld(clientX, clientY) {
    const rect = state.svg.getBoundingClientRect();
    return {
      x: (clientX - rect.left - state.view.x) / state.view.k,
      y: (clientY - rect.top - state.view.y) / state.view.k,
    };
  }

  const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

  function onWheel(e) {
    e.preventDefault(); // the canvas owns the wheel; never scroll the page
    state.autoFit = false;
    const rect = state.svg.getBoundingClientRect();
    const mx = e.clientX - rect.left, my = e.clientY - rect.top;
    const k2 = clamp(state.view.k * Math.exp(-e.deltaY * 0.0015), SCALE_MIN, SCALE_MAX);
    // Anchor the zoom on the cursor: the world point under it stays put.
    state.view.x = mx - (mx - state.view.x) * (k2 / state.view.k);
    state.view.y = my - (my - state.view.y) * (k2 / state.view.k);
    state.view.k = k2;
    updateViewTransform();
  }

  function zoomToFit() {
    if (!state.nodes.length) return;
    const rect = state.svg.getBoundingClientRect();
    if (!rect.width || !rect.height) return; // tab hidden; nothing to fit to
    let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
    for (const nd of state.nodes) {
      minX = Math.min(minX, nd.x - nd.r); maxX = Math.max(maxX, nd.x + nd.r);
      minY = Math.min(minY, nd.y - nd.r); maxY = Math.max(maxY, nd.y + nd.r);
    }
    const pad = 48;
    const w = Math.max(maxX - minX, 1), h = Math.max(maxY - minY, 1);
    const k = clamp(Math.min((rect.width - pad * 2) / w, (rect.height - pad * 2) / h),
                    SCALE_MIN, FIT_MAX_SCALE);
    state.view.k = k;
    state.view.x = rect.width / 2 - k * (minX + w / 2);
    state.view.y = rect.height / 2 - k * (minY + h / 2);
    updateViewTransform();
  }

  // ---------- pointer interactions ----------
  //
  // One pointer at a time: drag a node to reposition it (pinned while held,
  // physics gently reheated so neighbours adjust), drag the background to
  // pan. Less than CLICK_SLOP px of travel counts as a click instead:
  // select a node, or clear the selection on the background.

  function onPointerDown(e) {
    if (e.button !== 0) return;
    const nd = e.target.__node || null;
    state.svg.setPointerCapture(e.pointerId);
    if (nd) {
      const w = screenToWorld(e.clientX, e.clientY);
      state.drag = {
        mode: "node", node: nd,
        offX: nd.x - w.x, offY: nd.y - w.y, // grab offset, so the node doesn't jump to the cursor
        startX: e.clientX, startY: e.clientY, moved: 0,
      };
    } else {
      state.drag = {
        mode: "pan",
        viewX0: state.view.x, viewY0: state.view.y,
        startX: e.clientX, startY: e.clientY, moved: 0,
      };
      state.svg.classList.add("panning");
    }
  }

  function onPointerMove(e) {
    const d = state.drag;
    if (!d) return;
    d.moved = Math.max(d.moved, Math.hypot(e.clientX - d.startX, e.clientY - d.startY));
    if (d.moved > CLICK_SLOP) state.autoFit = false; // a real drag = user owns the camera now
    if (d.mode === "pan") {
      state.view.x = d.viewX0 + (e.clientX - d.startX);
      state.view.y = d.viewY0 + (e.clientY - d.startY);
      updateViewTransform();
    } else {
      const w = screenToWorld(e.clientX, e.clientY);
      d.node.x = w.x + d.offX;
      d.node.y = w.y + d.offY;
      d.node.vx = 0; d.node.vy = 0;
      startSim(DRAG_ALPHA); // mild reheat: neighbours follow without a full re-layout
    }
  }

  function onPointerUp(e) {
    const d = state.drag;
    if (!d) return;
    state.drag = null;
    state.svg.classList.remove("panning");
    if (d.moved <= CLICK_SLOP) {
      selectNode(d.mode === "node" ? d.node : null);
    }
  }

  function onDblClick(e) {
    if (!e.target.__node) zoomToFit(); // double-click the background = fit
  }

  function onKeyDown(e) {
    if (e.key === "Escape" && !$("#view-topology").hidden) selectNode(null);
  }

  // ---------- selection & details panel ----------

  function selectNode(nd) {
    const prev = state.selected;
    state.selected = nd;
    if (prev && prev.labelEl) syncOneLabel(prev);
    if (nd) {
      state.ring.setAttribute("r", nd.r + 4);
      state.ring.setAttribute("cx", nd.x);
      state.ring.setAttribute("cy", nd.y);
      state.ring.removeAttribute("display");
      syncOneLabel(nd);
    } else {
      state.ring.setAttribute("display", "none");
    }
    renderPanel();
  }

  // Selecting a peer from the panel also pans it to the viewport centre so
  // the jump is visible.
  function selectAndCenter(nd) {
    selectNode(nd);
    const rect = state.svg.getBoundingClientRect();
    state.view.x = rect.width / 2 - state.view.k * nd.x;
    state.view.y = rect.height / 2 - state.view.k * nd.y;
    updateViewTransform();
  }

  function renderPanel() {
    const panel = $("#topo-panel");
    const nd = state.selected;
    if (!nd) {
      panel.classList.remove("open");
      return;
    }
    const a = nd.asset;
    panel.replaceChildren();

    const close = el("button", { class: "topo-panel-close", type: "button", title: "Close" }, "×");
    close.addEventListener("click", () => selectNode(null));
    panel.appendChild(el("div", { class: "topo-panel-head" },
      el("h2", {}, a.name || a.id), close));

    const kv = el("dl", { class: "topo-kv" });
    for (const [label, val] of [
      ["Provider", a.provider],
      ["Type", a.type],
      ["Account", a.account_id],
      ["Region", a.region],
      ["Status", a.status],
      ["Created", a.created_at],
    ]) {
      if (!val) continue;
      kv.append(el("dt", {}, label), el("dd", {}, val));
    }
    panel.appendChild(kv);

    const copy = el("button", { type: "button", title: "Copy ID to clipboard" }, "Copy");
    copy.addEventListener("click", () => {
      navigator.clipboard.writeText(a.id).then(
        () => flashButton(copy, "Copied"),
        () => flashButton(copy, "Failed"),
      );
    });
    panel.appendChild(el("div", { class: "topo-id-row" }, el("code", {}, a.id), copy));

    const tags = Object.entries(a.tags || {});
    if (tags.length) {
      panel.appendChild(el("h3", {}, "Tags"));
      const table = el("table", { class: "topo-tags" });
      for (const [k, v] of tags.sort((x, y) => (x[0] < y[0] ? -1 : 1))) {
        table.appendChild(el("tr", {}, el("td", {}, k), el("td", {}, v)));
      }
      panel.appendChild(table);
    }

    const inEdges  = state.rawEdges.filter((e) => refKey(e.to) === nd.key);
    const outEdges = state.rawEdges.filter((e) => refKey(e.from) === nd.key);
    panel.appendChild(el("h3", {}, `Edges in (${inEdges.length})`));
    panel.appendChild(edgeList(inEdges, "from"));
    panel.appendChild(el("h3", {}, `Edges out (${outEdges.length})`));
    panel.appendChild(edgeList(outEdges, "to"));

    panel.classList.add("open");
  }

  // Edge rows for the panel; peerSide names the AssetRef field holding the
  // other endpoint. Peers that exist as nodes get a clickable link; the
  // rest fall back to the raw ref ID.
  function edgeList(edges, peerSide) {
    const ul = el("ul", { class: "topo-edge-list" });
    if (!edges.length) {
      ul.appendChild(el("li", { class: "muted" }, "none"));
      return ul;
    }
    for (const edge of edges) {
      const peerRef = edge[peerSide];
      const peer = state.byKey.get(refKey(peerRef));
      const known = Object.prototype.hasOwnProperty.call(EDGE_COLORS, edge.kind);
      const li = el("li", {},
        dotSwatch(known ? EDGE_COLORS[edge.kind] : EDGE_FALLBACK),
        el("span", {}, edge.kind),
        el("span", { class: "conf" }, edge.confidence));
      if (peer) {
        const btn = el("button", { class: "peer", type: "button" }, peer.asset.name || peer.asset.id);
        btn.addEventListener("click", () => selectAndCenter(peer));
        li.appendChild(btn);
      } else {
        li.appendChild(el("span", { class: "muted" }, peerRef.id));
      }
      ul.appendChild(li);
    }
    return ul;
  }

  function flashButton(btn, text) {
    const orig = btn.textContent;
    btn.textContent = text;
    setTimeout(() => { btn.textContent = orig; }, 1200);
  }

  // ---------- status & errors ----------

  function setTopoStatus(s) { $("#topo-status").textContent = s; }

  function addTopoError(msg) {
    const li = document.createElement("li");
    li.textContent = msg;
    $("#topo-errors-list").appendChild(li);
    $("#topo-errors").hidden = false;
  }

  function clearTopoErrors() {
    $("#topo-errors-list").replaceChildren();
    $("#topo-errors").hidden = true;
  }

  function truncate(s, n) {
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }

  // ---------- demo data (#demo) ----------
  //
  // A synthetic-but-plausible chain mirroring what the resolvers emit:
  // Cloudflare DNS / WAF ruleset / Tunnel → OCI load balancers → Kubernetes
  // Ingress → Services → Pods, with a mix of exact and heuristic edges
  // (plus one unknown "tunnel" kind to exercise the fallback colour and
  // marker). Same envelope as /api/v1/topology?format=json.

  const cf  = (type, id) => ({ provider: "cloudflare", account_id: "0a1b2c3d4e5f", type, id });
  const oci = (type, id) => ({ provider: "oci", account_id: "ocid1.tenancy.oc1..demo", type, id });
  const k8s = (type, id) => ({ provider: "kubernetes", account_id: "prod-cluster", type, id });

  const DEMO_GRAPH = {
    nodes: [
      { ...cf("zone", "zone-example-com"), name: "example.com", status: "active" },
      { ...cf("dns_record", "dns-www"), name: "www.example.com", status: "active",
        tags: { type: "A", content: "203.0.113.10", proxied: "true" } },
      { ...cf("dns_record", "dns-api"), name: "api.example.com", status: "active",
        tags: { type: "A", content: "203.0.113.10", proxied: "true" } },
      { ...cf("dns_record", "dns-app"), name: "app.example.com", status: "active",
        tags: { type: "A", content: "203.0.113.20" } },
      { ...cf("ruleset", "rs-waf"), name: "Managed WAF",
        tags: { phase: "http_request_firewall_managed" } },
      { ...cf("tunnel", "tun-1"), name: "prod-tunnel", status: "healthy" },

      { ...oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"), name: "lb-prod",
        region: "eu-frankfurt-1", status: "ACTIVE",
        tags: { ip: "203.0.113.10", shape: "flexible" } },
      { ...oci("load_balancer", "ocid1.loadbalancer.oc1..lbstaging"), name: "lb-staging",
        region: "eu-frankfurt-1", status: "ACTIVE", tags: { ip: "203.0.113.20" } },
      { ...oci("instance", "ocid1.instance.oc1..webvm1"), name: "web-vm-1",
        region: "eu-frankfurt-1", status: "RUNNING",
        tags: { shape: "VM.Standard.E4.Flex" } },

      { ...k8s("networking.k8s.io/v1.Ingress", "default/web"), name: "web",
        tags: { namespace: "default", class: "nginx" } },
      { ...k8s("v1.Service", "default/web"), name: "web", status: "active",
        tags: { namespace: "default", type: "LoadBalancer" } },
      { ...k8s("v1.Service", "default/api"), name: "api", status: "active",
        tags: { namespace: "default", type: "ClusterIP" } },
      { ...k8s("v1.Pod", "default/web-6f7d9-x2x4l"), name: "web-6f7d9-x2x4l", status: "Running",
        tags: { namespace: "default" } },
      { ...k8s("v1.Pod", "default/web-6f7d9-p9q1r"), name: "web-6f7d9-p9q1r", status: "Running",
        tags: { namespace: "default" } },
      { ...k8s("v1.Pod", "default/api-58c4b-zz9t7"), name: "api-58c4b-zz9t7", status: "Running",
        tags: { namespace: "default" } },
    ],
    edges: [
      { from: cf("dns_record", "dns-www"), to: oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"),
        kind: "dns", hostname: "www.example.com", confidence: "heuristic" },
      { from: cf("dns_record", "dns-api"), to: oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"),
        kind: "dns", hostname: "api.example.com", confidence: "heuristic" },
      { from: cf("dns_record", "dns-api"), to: k8s("v1.Service", "default/api"),
        kind: "dns", hostname: "api.example.com", confidence: "heuristic" },
      { from: cf("dns_record", "dns-app"), to: oci("load_balancer", "ocid1.loadbalancer.oc1..lbstaging"),
        kind: "dns", hostname: "app.example.com", confidence: "heuristic" },
      { from: cf("ruleset", "rs-waf"), to: cf("zone", "zone-example-com"),
        kind: "waf", confidence: "exact" },
      { from: cf("tunnel", "tun-1"), to: cf("zone", "zone-example-com"),
        kind: "waf", confidence: "exact" },
      { from: cf("tunnel", "tun-1"), to: k8s("v1.Service", "default/web"),
        kind: "tunnel", confidence: "heuristic" }, // unknown kind → fallback colour
      { from: oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"), to: k8s("v1.Service", "default/web"),
        kind: "lb-backend", confidence: "heuristic" },
      { from: oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"), to: k8s("v1.Service", "default/api"),
        kind: "lb-backend", confidence: "heuristic" },
      { from: oci("load_balancer", "ocid1.loadbalancer.oc1..lbprod"), to: oci("instance", "ocid1.instance.oc1..webvm1"),
        kind: "lb-backend", port: 443, confidence: "exact" },
      { from: oci("load_balancer", "ocid1.loadbalancer.oc1..lbstaging"), to: oci("instance", "ocid1.instance.oc1..webvm1"),
        kind: "lb-backend", port: 8080, confidence: "exact" },
      { from: k8s("networking.k8s.io/v1.Ingress", "default/web"), to: k8s("v1.Service", "default/web"),
        kind: "gateway-route", hostname: "www.example.com", port: 80, confidence: "exact" },
      { from: k8s("networking.k8s.io/v1.Ingress", "default/web"), to: k8s("v1.Service", "default/api"),
        kind: "gateway-route", hostname: "api.example.com", port: 8080, confidence: "exact" },
      { from: k8s("v1.Service", "default/web"), to: k8s("v1.Pod", "default/web-6f7d9-x2x4l"),
        kind: "service-backend", confidence: "exact" },
      { from: k8s("v1.Service", "default/web"), to: k8s("v1.Pod", "default/web-6f7d9-p9q1r"),
        kind: "service-backend", confidence: "exact" },
      { from: k8s("v1.Service", "default/api"), to: k8s("v1.Pod", "default/api-58c4b-zz9t7"),
        kind: "service-backend", confidence: "exact" },
    ],
  };
})();
