// Cloud Asset Auditor — Dashboard tab. Live summary charts over the assets
// streamed on the Assets tab. Vanilla JS only: the project rule is
// "no third-party JS, no build step", so the donut and bar charts below are
// hand-rolled SVG (and deliberately small).
//
// Data flows one way: app.js dispatches a throttled "auditor:assets"
// CustomEvent while an audit streams (every 100th asset, plus once on done);
// we re-read window.auditorShared.assets() and rebuild every chart
// wholesale — the same philosophy as the Assets table's full-body re-render:
// fast enough at 50k rows and dead simple. While the tab is hidden we only
// mark ourselves dirty and catch up when it's activated.
//
// Click-through is the inverse hop: donut segments / legend rows and type
// bars call window.auditorShared.showAssetsFacet; region and account bars
// call showAssetsFiltered (region/account aren't facets — the substring
// filter is the closest thing the Assets tab has).

(() => {
  "use strict";

  const $ = (sel, root = document) => root.querySelector(sel);

  // Same provider palette as topology.js, so the two tabs agree on colours.
  // Providers we don't know cycle through the fallbacks by donut position.
  const PROVIDER_COLORS = {
    cloudflare: "#f6821f",
    oci:        "#c74634",
    kubernetes: "#326ce5",
  };
  const FALLBACK_COLORS = ["#8b5cf6", "#10b981", "#f59e0b", "#14b8a6", "#ec4899", "#6b7280"];

  const TOP_N = 15;            // bar charts show at most this many rows
  const LABEL_MAX_CHARS = 32;  // bar labels (K8s GVK types get long)

  let dirty = true; // assets changed while the tab was hidden → rebuild on activation

  // ---------- bootstrap ----------

  function init() {
    // Fired by app.js on the facet-rebuild cadence. Render immediately only
    // when visible; a hidden rebuild would be wasted work every 100 assets.
    document.addEventListener("auditor:assets", () => {
      if ($("#view-dashboard").hidden) { dirty = true; return; }
      render();
    });

    // app.js's activateTab click handler registered first (its script tag
    // precedes ours), so by the time this runs the view is already visible.
    $("#tab-dashboard").addEventListener("click", () => { if (dirty) render(); });

    render(); // initial paint = the empty state
  }

  document.addEventListener("DOMContentLoaded", init);

  // ---------- aggregation ----------

  // Count assets per distinct non-empty value of `key`, sorted by count desc
  // then value asc — the same ordering renderFacet uses, so the Dashboard
  // and the facet rail always agree and re-renders are deterministic for
  // the same data.
  function countBy(assets, key) {
    const counts = new Map();
    for (const a of assets) {
      const v = a[key];
      if (v == null || v === "") continue;
      counts.set(v, (counts.get(v) || 0) + 1);
    }
    return Array.from(counts.entries()).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
  }

  // ---------- render ----------

  function render() {
    dirty = false;
    const assets = window.auditorShared.assets();

    const empty = assets.length === 0;
    $("#dash-empty").hidden = !empty;
    $("#dash-content").hidden = empty;
    if (empty) return;

    const byProvider = countBy(assets, "provider");
    const byType     = countBy(assets, "type");
    const byRegion   = countBy(assets, "region");
    const byAccount  = countBy(assets, "account_id");

    renderStats(assets.length, byProvider, byType, byRegion, byAccount);
    renderDonut(byProvider, assets.length);
    renderBars("#dash-types", byType, (v) => window.auditorShared.showAssetsFacet("type", v));

    // Region/account cards disappear entirely when no asset carries the
    // field (e.g. a pure-Kubernetes audit has no regions).
    toggleCard("#dash-card-regions",  "#dash-regions",  byRegion,
               (v) => window.auditorShared.showAssetsFiltered(v));
    toggleCard("#dash-card-accounts", "#dash-accounts", byAccount,
               (v) => window.auditorShared.showAssetsFiltered(v));
  }

  function toggleCard(cardSel, chartSel, entries, onPick) {
    const card = $(cardSel);
    card.hidden = entries.length === 0;
    if (entries.length) renderBars(chartSel, entries, onPick);
  }

  // ---------- stat pills ----------

  function renderStats(total, byProvider, byType, byRegion, byAccount) {
    const host = $("#dash-stats");
    host.replaceChildren();
    for (const [value, noun] of [
      [total,             "asset"],
      [byProvider.length, "provider"],
      [byType.length,     "type"],
      [byRegion.length,   "region"],
      [byAccount.length,  "account"],
    ]) {
      host.appendChild(el("div", { class: "dash-stat" },
        el("span", { class: "dash-stat-value" }, String(value)),
        el("span", { class: "dash-stat-label" }, value === 1 ? noun : noun + "s")));
    }
  }

  // ---------- donut ----------

  const DONUT_SIZE = 180, DONUT_R = 80, DONUT_HOLE = 52;

  function renderDonut(entries, total) {
    const host = $("#dash-donut");
    host.replaceChildren();

    const cx = DONUT_SIZE / 2, cy = DONUT_SIZE / 2;
    const svg = svgEl("svg", {
      class: "dash-donut-svg",
      viewBox: `0 0 ${DONUT_SIZE} ${DONUT_SIZE}`,
      role: "img",
    });
    // Fractions come from the entries' own sum, not `total`, so the ring
    // always closes even if some asset lacked a provider value.
    const sum = entries.reduce((n, [, c]) => n + c, 0) || 1;
    const legend = el("div", { class: "dash-legend" });

    let angle = -Math.PI / 2; // start at 12 o'clock, sweep clockwise
    entries.forEach(([provider, count], i) => {
      const color = PROVIDER_COLORS[provider] || FALLBACK_COLORS[i % FALLBACK_COLORS.length];
      const pct = ((count / sum) * 100).toFixed(1) + "%";
      const onPick = () => window.auditorShared.showAssetsFacet("provider", provider);

      let shape;
      if (entries.length === 1) {
        // 100% case: an arc whose start and end coincide collapses to
        // nothing, so the full ring is a stroked circle instead of two
        // degenerate A commands.
        shape = svgEl("circle", {
          class: "dash-arc",
          cx, cy, r: (DONUT_R + DONUT_HOLE) / 2,
          fill: "none", stroke: color, "stroke-width": DONUT_R - DONUT_HOLE,
        });
      } else {
        const a0 = angle, a1 = angle + (count / sum) * 2 * Math.PI;
        angle = a1;
        shape = svgEl("path", { class: "dash-arc", d: arcPath(cx, cy, a0, a1), fill: color });
      }
      const tip = svgEl("title");
      tip.textContent = `${provider} — ${count} (${pct})`;
      shape.appendChild(tip);
      shape.addEventListener("click", onPick);
      svg.appendChild(shape);

      const dot = el("span", { class: "dot" });
      dot.style.background = color;
      const row = el("div", { class: "dash-legend-row", title: `${count} (${pct})` },
        dot, el("span", {}, provider), el("span", { class: "count" }, `${count} · ${pct}`));
      row.addEventListener("click", onPick);
      legend.appendChild(row);
    });

    const num = svgEl("text", { class: "dash-donut-total", x: cx, y: cy - 2, "text-anchor": "middle" });
    num.textContent = String(total);
    const cap = svgEl("text", { class: "dash-donut-caption", x: cx, y: cy + 16, "text-anchor": "middle" });
    cap.textContent = "assets";
    svg.append(num, cap);

    host.append(svg, legend);
  }

  // Annular sector from angle a0 to a1 (radians, clockwise): outer arc out,
  // straight edge in, inner arc back. large-arc flips when the slice spans
  // more than half the ring.
  function arcPath(cx, cy, a0, a1) {
    const large = a1 - a0 > Math.PI ? 1 : 0;
    const x = (r, a) => cx + r * Math.cos(a);
    const y = (r, a) => cy + r * Math.sin(a);
    return `M ${x(DONUT_R, a0)} ${y(DONUT_R, a0)} ` +
           `A ${DONUT_R} ${DONUT_R} 0 ${large} 1 ${x(DONUT_R, a1)} ${y(DONUT_R, a1)} ` +
           `L ${x(DONUT_HOLE, a1)} ${y(DONUT_HOLE, a1)} ` +
           `A ${DONUT_HOLE} ${DONUT_HOLE} 0 ${large} 0 ${x(DONUT_HOLE, a0)} ${y(DONUT_HOLE, a0)} Z`;
  }

  // ---------- bar charts ----------
  //
  // One row per entry: right-aligned label, proportional rect, count. Fixed
  // viewBox coordinates (the SVG stretches to its container) keep the maths
  // trivial and the markup deterministic.

  const BAR_W = 640, ROW_H = 24, BAR_H = 15;
  const LABEL_W = 230, COUNT_W = 56, BAR_X = LABEL_W + 10;
  const BAR_MAX = BAR_W - BAR_X - COUNT_W;

  function renderBars(sel, entries, onPick) {
    const host = $(sel);
    host.replaceChildren();

    const rows = entries.slice(0, TOP_N);
    const max = rows[0][1]; // entries are count-desc, so the first is the max
    const svg = svgEl("svg", {
      class: "dash-bars",
      viewBox: `0 0 ${BAR_W} ${rows.length * ROW_H}`,
      role: "img",
    });

    rows.forEach(([name, count], i) => {
      const yMid = i * ROW_H + ROW_H / 2;
      const w = Math.max((count / max) * BAR_MAX, 2); // floor: tiny counts stay visible
      const g = svgEl("g", { class: "dash-bar-row" });

      const tip = svgEl("title");
      tip.textContent = `${name} — ${count}`;
      g.appendChild(tip);

      // Invisible full-width rect so the whole row — not just the bar — is
      // the click/hover target.
      g.appendChild(svgEl("rect", { class: "dash-bar-hit", x: 0, y: i * ROW_H, width: BAR_W, height: ROW_H }));

      const label = svgEl("text", { class: "dash-bar-label", x: LABEL_W, y: yMid, "text-anchor": "end" });
      label.textContent = truncate(name, LABEL_MAX_CHARS);
      g.appendChild(label);

      g.appendChild(svgEl("rect", {
        class: "dash-bar", x: BAR_X, y: i * ROW_H + (ROW_H - BAR_H) / 2,
        width: w, height: BAR_H, rx: 2,
      }));

      const num = svgEl("text", { class: "dash-bar-count", x: BAR_X + w + 6, y: yMid });
      num.textContent = String(count);
      g.appendChild(num);

      g.addEventListener("click", () => onPick(name));
      svg.appendChild(g);
    });

    host.appendChild(svg);
    if (entries.length > TOP_N) {
      host.appendChild(el("div", { class: "dash-more muted" }, `+ ${entries.length - TOP_N} more not shown`));
    }
  }

  // ---------- tiny DOM helpers (same shape as topology.js's) ----------

  function svgEl(tag, attrs = {}) {
    const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs)) node.setAttribute(k, v);
    return node;
  }

  function el(tag, attrs = {}, ...children) {
    const node = document.createElement(tag);
    for (const [k, v] of Object.entries(attrs)) {
      if (k === "class") node.className = v;
      else node.setAttribute(k, v);
    }
    node.append(...children);
    return node;
  }

  function truncate(s, n) {
    return s.length > n ? s.slice(0, n - 1) + "…" : s;
  }
})();
