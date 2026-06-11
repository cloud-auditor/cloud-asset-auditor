// Cloud Asset Auditor — embedded SPA, vanilla JS, no build step.
//
// app.js owns the Assets tab (and the tab bar itself); topology.js owns the
// Topology tab and reads window.auditorShared (defined at the bottom).
//
// State machine (intentionally tiny):
//   idle  ──run──▶ running  ──done/stop/error──▶ idle
//
// All assets collected during a run live in `state.assets`. The visible
// table is `state.assets` filtered by `state.filter`, the active facet
// (`state.activeFacet`), and sorted by `state.sortKey` / `state.sortDir`.
// Whenever any of those change we re-render the body wholesale — it's
// fast enough up to ~50k rows and keeps the model dead simple.

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

const COLUMNS = ["provider", "type", "account_id", "region", "name", "status", "id"];

const state = {
  providers: [],          // discovered from /api/v1/providers
  selected:  new Set(),   // user-selected subset
  assets:    [],          // every Asset received this run
  filter:    "",
  activeFacet: { kind: null, value: null }, // {kind:"type"|"provider", value:"v1.Pod"} or {nulls}
  sortKey:   "provider",
  sortDir:   "asc",
  source:    null,        // active EventSource, or null
  startedAt: 0,
};

// ---------- bootstrap ----------

async function init() {
  initTabs();
  checkHealth();
  setInterval(checkHealth, 10_000);

  try {
    const r = await fetch("/api/v1/providers");
    const j = await r.json();
    state.providers = j.providers || [];
    j.providers.forEach((p) => state.selected.add(p));
    renderProviders();
  } catch (e) {
    setStatus(`Failed to load providers: ${e.message}`);
  }

  $("#run-btn").addEventListener("click", runAudit);
  $("#stop-btn").addEventListener("click", stopAudit);
  $("#filter").addEventListener("input", (e) => {
    state.filter = e.target.value.trim().toLowerCase();
    rerender();
  });
  $$("#assets thead th").forEach((th) => th.addEventListener("click", () => sortBy(th.dataset.key)));
  $("#export-csv-btn").addEventListener("click",  () => exportTo("csv"));
  $("#export-json-btn").addEventListener("click", () => exportTo("json"));
}

document.addEventListener("DOMContentLoaded", init);

// ---------- tabs ----------

function initTabs() {
  $$(".tabs .tab").forEach((btn) => btn.addEventListener("click", () => activateTab(btn)));
}

// Plain show/hide: both views stay in the DOM, so each tab keeps its state
// (table rows, graph positions, scroll) across switches.
function activateTab(btn) {
  $$(".tabs .tab").forEach((b) => b.classList.toggle("active", b === btn));
  $$(".view").forEach((v) => { v.hidden = v.id !== btn.dataset.view; });
}

// ---------- health ----------

async function checkHealth() {
  const pill = $("#health-pill");
  try {
    const r = await fetch("/healthz");
    if (r.ok) {
      pill.textContent = "healthy";
      pill.className = "pill pill-ok";
    } else {
      pill.textContent = `unhealthy ${r.status}`;
      pill.className = "pill pill-error";
    }
  } catch {
    pill.textContent = "unreachable";
    pill.className = "pill pill-error";
  }
}

// ---------- audit ----------

function runAudit() {
  if (state.source) return; // already running
  state.assets = [];
  state.startedAt = Date.now();
  $("#errors").hidden = true;
  $("#errors-list").innerHTML = "";

  $("#run-btn").hidden = true;
  $("#stop-btn").hidden = false;

  const params = new URLSearchParams();
  const selected = Array.from(state.selected);
  if (selected.length > 0 && selected.length < state.providers.length) {
    params.set("providers", selected.join(","));
  }

  const es = new EventSource("/api/v1/audit?" + params.toString());
  state.source = es;
  setStatus("Starting…");

  es.addEventListener("meta", () => setStatus("Streaming…"));
  es.addEventListener("init_error", (e) => {
    const o = JSON.parse(e.data);
    addError(`init: ${o.message}`);
  });
  es.addEventListener("asset", (e) => {
    state.assets.push(JSON.parse(e.data));
    rerender({ tableOnly: state.assets.length % 100 !== 0 }); // throttle facet rebuilds
    setStatus(`${state.assets.length} assets streamed…`);
  });
  es.addEventListener("error", (e) => {
    // Server-emitted application error (not EventSource transport error).
    if (e.data) {
      try { addError(JSON.parse(e.data).message); } catch { addError(String(e.data)); }
    }
  });
  es.addEventListener("done", (e) => {
    const o = JSON.parse(e.data);
    const elapsed = ((Date.now() - state.startedAt) / 1000).toFixed(1);
    setStatus(`Done. ${o.count} assets in ${elapsed}s` + (o.errors ? ` (${o.errors} errors)` : ""));
    rerender();
    stopAudit();
  });
  es.onerror = () => {
    // Transport-level error (server closed connection without `done`,
    // or network failure). Close to suppress auto-reconnect.
    if (state.source) {
      addError("connection closed unexpectedly");
      stopAudit();
    }
  };
}

function stopAudit() {
  if (state.source) {
    state.source.close();
    state.source = null;
  }
  $("#run-btn").hidden = false;
  $("#stop-btn").hidden = true;
}

function exportTo(format) {
  const params = new URLSearchParams({ format });
  const selected = Array.from(state.selected);
  if (selected.length > 0 && selected.length < state.providers.length) {
    params.set("providers", selected.join(","));
  }
  window.location.href = "/api/v1/audit/export?" + params.toString();
}

// ---------- rendering ----------

function rerender(opts = {}) {
  const filtered = visibleAssets();
  renderTable(filtered);
  if (!opts.tableOnly) renderFacets();
}

function visibleAssets() {
  let out = state.assets;

  if (state.activeFacet.kind) {
    out = out.filter((a) => a[state.activeFacet.kind] === state.activeFacet.value);
  }

  if (state.filter) {
    const f = state.filter;
    out = out.filter((a) => COLUMNS.some((c) => String(a[c] ?? "").toLowerCase().includes(f)));
  }

  const k = state.sortKey;
  const dir = state.sortDir === "asc" ? 1 : -1;
  return [...out].sort((a, b) => {
    const av = String(a[k] ?? ""), bv = String(b[k] ?? "");
    return av < bv ? -dir : av > bv ? dir : 0;
  });
}

function renderTable(rows) {
  const body = $("#assets-body");
  const frag = document.createDocumentFragment();
  for (const a of rows) {
    const tr = document.createElement("tr");
    for (const c of COLUMNS) {
      const td = document.createElement("td");
      const v = a[c];
      if (v == null || v === "") {
        td.className = "muted";
        td.textContent = "—";
      } else {
        td.textContent = v;
      }
      tr.appendChild(td);
    }
    frag.appendChild(tr);
  }
  body.replaceChildren(frag);

  $$("#assets thead th").forEach((th) => {
    th.classList.remove("sort-asc", "sort-desc");
    if (th.dataset.key === state.sortKey) {
      th.classList.add(state.sortDir === "asc" ? "sort-asc" : "sort-desc");
    }
  });
}

function renderProviders() {
  const host = $("#providers-list");
  host.replaceChildren();
  for (const p of state.providers) {
    const id = `provider-${p}`;
    const label = document.createElement("label");
    label.htmlFor = id;
    const cb = document.createElement("input");
    cb.type = "checkbox";
    cb.id = id;
    cb.checked = state.selected.has(p);
    cb.addEventListener("change", () => {
      if (cb.checked) state.selected.add(p);
      else state.selected.delete(p);
    });
    label.appendChild(cb);
    label.appendChild(document.createTextNode(" " + p));
    host.appendChild(label);
  }
}

function renderFacets() {
  renderFacet("#facet-types",     "type",     state.assets);
  renderFacet("#facet-providers", "provider", state.assets);
}

function renderFacet(sel, key, assets) {
  const counts = new Map();
  for (const a of assets) {
    const v = a[key] || "(empty)";
    counts.set(v, (counts.get(v) || 0) + 1);
  }
  const entries = Array.from(counts.entries()).sort((a, b) => b[1] - a[1] || (a[0] < b[0] ? -1 : 1));
  const host = $(sel);
  host.replaceChildren();
  const allLi = facetItem("All", assets.length, () => clearFacet(key));
  allLi.classList.toggle("active", state.activeFacet.kind !== key);
  host.appendChild(allLi);
  for (const [val, n] of entries) {
    const li = facetItem(val, n, () => setFacet(key, val));
    if (state.activeFacet.kind === key && state.activeFacet.value === val) {
      li.classList.add("active");
    }
    host.appendChild(li);
  }
}

function facetItem(label, count, onclick) {
  const li = document.createElement("li");
  const name = document.createElement("span");
  name.textContent = label;
  const c = document.createElement("span");
  c.className = "count";
  c.textContent = count;
  li.append(name, c);
  li.addEventListener("click", onclick);
  return li;
}

function setFacet(kind, value) {
  state.activeFacet = { kind, value };
  rerender();
}
function clearFacet() {
  state.activeFacet = { kind: null, value: null };
  rerender();
}

function sortBy(key) {
  if (!key) return;
  if (state.sortKey === key) {
    state.sortDir = state.sortDir === "asc" ? "desc" : "asc";
  } else {
    state.sortKey = key;
    state.sortDir = "asc";
  }
  rerender({ tableOnly: true });
}

// ---------- misc ----------

function setStatus(s) { $("#status").textContent = s; }

function addError(msg) {
  const ul = $("#errors-list");
  const li = document.createElement("li");
  li.textContent = msg;
  ul.appendChild(li);
  $("#errors").hidden = false;
}

// ---------- shared surface ----------
//
// topology.js (the second embedded script) reads the Assets tab's provider
// selection so "Build graph" targets the same subset as "Run audit".
// Getters that copy — never the live Set/array — keep the coupling
// read-only and one-way.
window.auditorShared = {
  selectedProviders: () => Array.from(state.selected),
  allProviders:      () => state.providers.slice(),
  assets:            () => state.assets.slice(),
};
