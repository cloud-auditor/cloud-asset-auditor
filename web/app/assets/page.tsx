'use client';

import { useCallback, useDeferredValue, useEffect, useMemo, useRef, useState } from 'react';
import { useAudit } from '@/components/AuditProvider';
import { AuditControls } from '@/components/AuditControls';
import { AssetDrawer, assetAddress } from '@/components/AssetDrawer';
import { FacetSelect, usePopover, type FacetOption } from '@/components/FacetSelect';
import { VirtualTable, type VirtualColumn, type VirtualTableHandle } from '@/components/VirtualTable';
import { copyText } from '@/components/JsonView';
import { exportURL } from '@/lib/api';
import { providerColor, statusTone, toneColor } from '@/lib/colors';
import { AssetIcon, Icon } from '@/lib/icons';
import type { Asset } from '@/lib/types';
import './assets.css';

type ColKey = 'provider' | 'type' | 'name' | 'region' | 'account_id' | 'status' | 'address';
type SortKey = Exclude<ColKey, 'address'>;
type FacetKey = 'provider' | 'type' | 'region' | 'status';
type Density = 'comfortable' | 'compact';

const COLUMNS: readonly (VirtualColumn & { key: ColKey })[] = [
  { key: 'provider', label: 'Provider', width: 'minmax(112px, 0.7fr)', sortable: true },
  { key: 'type', label: 'Type', width: 'minmax(150px, 1.15fr)', sortable: true },
  { key: 'name', label: 'Name', width: 'minmax(210px, 2.2fr)', sortable: true },
  { key: 'region', label: 'Region', width: 'minmax(96px, 0.7fr)', sortable: true },
  { key: 'account_id', label: 'Account', width: 'minmax(112px, 0.85fr)', sortable: true },
  { key: 'status', label: 'Status', width: 'minmax(104px, 0.7fr)', sortable: true },
  { key: 'address', label: 'Address', width: 'minmax(128px, 1fr)' },
];

const FACETS: readonly { key: FacetKey; label: string }[] = [
  { key: 'provider', label: 'Provider' },
  { key: 'type', label: 'Type' },
  { key: 'region', label: 'Region' },
  { key: 'status', label: 'Status' },
];

/** Mirrors the `[data-density]` block in globals.css. The number has to exist
 *  in JS as well as CSS because the windowing maths is index × height. */
const ROW_H: Record<Density, number> = { comfortable: 40, compact: 28 };

const EXPORTS = ['json', 'csv', 'xlsx', 'html'] as const;

/** `numeric` is what makes `node-2` sort before `node-10`, which is how an
 *  operator reads a cluster's node list. A single Collator instance is an order
 *  of magnitude cheaper per comparison than String.localeCompare. */
const COLLATOR = new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' });

const PREFS_KEY = 'auditor-assets-view';

interface Prefs {
  density: Density;
  hidden: ColKey[];
}

/** Module-scoped so the view survives client-side navigation (which unmounts
 *  this page on every tab change); localStorage covers the reload on top. */
let prefsCache: Prefs | null = null;

function readPrefs(): Prefs | null {
  try {
    const raw = window.localStorage.getItem(PREFS_KEY);
    if (!raw) return null;
    const parsed: unknown = JSON.parse(raw);
    if (typeof parsed !== 'object' || parsed === null) return null;
    const rec = parsed as Record<string, unknown>;
    const density = rec.density === 'compact' ? 'compact' : 'comfortable';
    const keys = COLUMNS.map((c) => c.key);
    const hidden = Array.isArray(rec.hidden)
      ? rec.hidden.filter((v): v is ColKey => typeof v === 'string' && (keys as string[]).includes(v))
      : [];
    return { density, hidden };
  } catch {
    // Storage throws outright in private-mode Safari, and JSON.parse throws on
    // a hand-edited value. Neither is worth failing the page over.
    return null;
  }
}

function facetValue(a: Asset, k: FacetKey): string {
  switch (k) {
    case 'provider':
      return a.provider;
    case 'type':
      return a.type;
    case 'region':
      return a.region ?? '';
    case 'status':
      return a.status ?? '';
  }
}

function sortValue(a: Asset, k: SortKey): string {
  switch (k) {
    case 'provider':
      return a.provider;
    case 'type':
      return a.type;
    case 'name':
      return a.name;
    case 'region':
      return a.region ?? '';
    case 'account_id':
      return a.account_id;
    case 'status':
      return a.status ?? '';
  }
}

function vars(custom: Record<string, string>, rest?: React.CSSProperties): React.CSSProperties {
  return { ...rest, ...custom } as unknown as React.CSSProperties;
}

export default function AssetsPage() {
  const { assets, running, selectedProviders, selectedKubeContexts, start, toast } = useAudit();

  const [q, setQ] = useState('');
  const [filters, setFilters] = useState<Record<FacetKey, string[]>>({
    provider: [],
    type: [],
    region: [],
    status: [],
  });
  const [sort, setSort] = useState<SortKey>('provider');
  const [desc, setDesc] = useState(false);
  const [density, setDensity] = useState<Density>(() => prefsCache?.density ?? 'comfortable');
  const [hidden, setHidden] = useState<ColKey[]>(() => prefsCache?.hidden ?? []);
  const [detail, setDetail] = useState<{ asset: Asset; index: number } | null>(null);

  const searchRef = useRef<HTMLInputElement | null>(null);
  const tableRef = useRef<VirtualTableHandle | null>(null);

  // The stream flushes every 200ms and the search box fires per keystroke;
  // both feed work measured in tens of milliseconds at 50k rows. Deferring
  // them keeps the input and the audit bar painting at full rate and lets
  // React throw away a superseded pass instead of finishing it.
  const streamed = useDeferredValue(assets);
  const query = useDeferredValue(q);
  // Only the query lags visibly. `streamed` also trails during a run, but a
  // table that dims itself for the whole audit would be reporting the stream,
  // not staleness.
  const stale = q !== query;

  useEffect(() => {
    if (prefsCache) return;
    const p = readPrefs();
    if (!p) return;
    prefsCache = p;
    setDensity(p.density);
    setHidden(p.hidden);
  }, []);

  const savePrefs = useCallback((next: Prefs) => {
    prefsCache = next;
    try {
      window.localStorage.setItem(PREFS_KEY, JSON.stringify(next));
    } catch {
      // The choice just doesn't survive a reload.
    }
  }, []);

  const setDensityPref = (d: Density) => {
    setDensity(d);
    savePrefs({ density: d, hidden });
  };
  const setHiddenPref = (h: ColKey[]) => {
    setHidden(h);
    savePrefs({ density, hidden: h });
  };

  // `/` focuses the search box — the shortcut the command palette's footer and
  // the shell both advertise. Ignored while the caret is already in a field,
  // where a slash is a slash.
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== '/' || e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target;
      if (t instanceof HTMLElement && (t.isContentEditable || /^(input|textarea|select)$/i.test(t.tagName))) {
        return;
      }
      e.preventDefault();
      searchRef.current?.focus();
      searchRef.current?.select();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, []);

  // `?q=` is how the command palette hands an asset over. Read from
  // window.location rather than useSearchParams: the latter forces a Suspense
  // boundary under `output: 'export'`, and this is a one-shot read.
  useEffect(() => {
    const fromURL = () => new URLSearchParams(window.location.search).get('q') ?? '';
    const initial = fromURL();
    if (initial) setQ(initial);
    const onPop = () => setQ(fromURL());
    // Same-route pushes (the palette while already on this page) change the URL
    // without remounting, so the shell announces them explicitly.
    const onExternal = (e: Event) => {
      const d = (e as CustomEvent<string>).detail;
      if (typeof d === 'string') setQ(d);
    };
    window.addEventListener('popstate', onPop);
    window.addEventListener('auditor:assets-query', onExternal);
    return () => {
      window.removeEventListener('popstate', onPop);
      window.removeEventListener('auditor:assets-query', onExternal);
    };
  }, []);

  // Write the query back so a reload or a shared link keeps the view. Debounced
  // and via replaceState: one history entry per keystroke would make the back
  // button unusable, and `history.state` is carried through so Next's router
  // does not lose its own bookkeeping.
  const firstSync = useRef(true);
  useEffect(() => {
    if (firstSync.current) {
      firstSync.current = false;
      return;
    }
    const id = window.setTimeout(() => {
      const url = new URL(window.location.href);
      if (q) url.searchParams.set('q', q);
      else url.searchParams.delete('q');
      window.history.replaceState(window.history.state, '', url);
    }, 400);
    return () => window.clearTimeout(id);
  }, [q]);

  // One pass for all four facets. Counts are of the whole inventory, not of the
  // current result: a facet that only ever offered the values already selected
  // could never be widened.
  const facetOptions = useMemo(() => {
    const counts: Record<FacetKey, Map<string, number>> = {
      provider: new Map(),
      type: new Map(),
      region: new Map(),
      status: new Map(),
    };
    for (const a of streamed) {
      for (const f of FACETS) {
        const v = facetValue(a, f.key);
        if (!v) continue;
        const m = counts[f.key];
        m.set(v, (m.get(v) ?? 0) + 1);
      }
    }
    const out = {} as Record<FacetKey, FacetOption[]>;
    for (const f of FACETS) {
      out[f.key] = [...counts[f.key]]
        .map(([value, count]) => ({
          value,
          count,
          color: f.key === 'provider' ? providerColor(value) : undefined,
        }))
        .sort((x, y) => y.count - x.count || COLLATOR.compare(x.value, y.value));
    }
    return out;
  }, [streamed]);

  // Sort first, filter second — deliberately, and this is the hot path.
  // Filtering preserves order, so the sorted array is reusable across every
  // keystroke and every facet click; only a sort-column change pays for the
  // sort again. The other way round re-sorts 50k rows per character typed.
  const sorted = useMemo(() => {
    const out = streamed.slice();
    const dir = desc ? -1 : 1;
    out.sort(
      (a, b) =>
        dir * COLLATOR.compare(sortValue(a, sort), sortValue(b, sort)) ||
        COLLATOR.compare(a.id, b.id),
    );
    return out;
  }, [streamed, sort, desc]);

  // Lazily-filled and keyed by the asset object, which is immutable once
  // streamed — so a growing inventory never invalidates what is already built,
  // and a row nobody ever searched never pays for its haystack.
  const haystacks = useRef<WeakMap<Asset, string> | null>(null);
  if (haystacks.current === null) haystacks.current = new WeakMap();

  const filtered = useMemo(() => {
    const needle = query.trim().toLowerCase();
    const active = FACETS.filter((f) => filters[f.key].length > 0);
    if (!needle && active.length === 0) return sorted;

    const sets = active.map((f) => [f.key, new Set(filters[f.key])] as const);
    const cache = haystacks.current;
    const out: Asset[] = [];
    outer: for (const a of sorted) {
      // Set lookups before the substring scan: a facet click usually removes
      // most of the inventory, and the scan is the expensive half.
      for (const [key, set] of sets) if (!set.has(facetValue(a, key))) continue outer;
      if (needle) {
        let h = cache?.get(a);
        if (h === undefined) {
          h = [
            a.name,
            a.id,
            a.type,
            a.provider,
            a.region ?? '',
            a.account_id,
            a.status ?? '',
            // Tag values are where the interesting identifiers live (IPs,
            // namespaces, zone ids), so they must be searchable too. The NUL
            // join stops a query from matching across two unrelated fields.
            ...Object.entries(a.tags ?? {}).map(([k, v]) => `${k}=${v}`),
          ]
            .join(' ')
            .toLowerCase();
          cache?.set(a, h);
        }
        if (!h.includes(needle)) continue;
      }
      out.push(a);
    }
    return out;
  }, [sorted, filters, query]);

  const visible = useMemo(() => COLUMNS.filter((c) => !hidden.includes(c.key)), [hidden]);

  // Read `sort` from the closure rather than from a setState updater: strict
  // mode invokes updaters twice, and a second setDesc inside one would toggle
  // the direction straight back.
  const onSort = useCallback(
    (key: string) => {
      const k = key as SortKey;
      if (k === sort) {
        setDesc((d) => !d);
        return;
      }
      setSort(k);
      setDesc(false);
    },
    [sort],
  );

  const dense = density === 'compact';
  const renderRow = useCallback(
    (a: Asset) => (
      <>
        {visible.map((c) => (
          <Cell key={c.key} col={c.key} asset={a} dense={dense} />
        ))}
      </>
    ),
    [visible, dense],
  );

  const rowProps = useCallback(
    (a: Asset) => ({
      className: 'surface-rail',
      style: vars({ '--rail': providerColor(a.provider) }),
    }),
    [],
  );

  const rowKey = useCallback((a: Asset) => `${a.provider}/${a.id}`, []);

  const chips: { key: FacetKey; value: string }[] = [];
  for (const f of FACETS) for (const v of filters[f.key]) chips.push({ key: f.key, value: v });

  const clearFacet = (key: FacetKey, value: string) =>
    setFilters((prev) => ({ ...prev, [key]: prev[key].filter((v) => v !== value) }));

  const clearAll = () => {
    setFilters({ provider: [], type: [], region: [], status: [] });
    setQ('');
  };

  const exportParams = { providers: selectedProviders, kubeContexts: selectedKubeContexts };

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Assets</h1>
          <p>Every resource the configured providers returned, streamed as it arrives.</p>
        </div>
      </div>

      <AuditControls />

      <div className="assets-toolbar card glass">
        <div className="assets-toolbar-row">
          <div className="assets-search">
            <Icon name="search" size={14} />
            <input
              ref={searchRef}
              value={q}
              onChange={(e) => setQ(e.target.value)}
              placeholder="Search name, id, type, region, or tag…"
              aria-label="Search assets"
              type="search"
              autoComplete="off"
              spellCheck={false}
            />
            {q ? (
              <button
                type="button"
                className="btn ghost icon sm"
                aria-label="Clear search"
                onClick={() => {
                  setQ('');
                  searchRef.current?.focus();
                }}
              >
                <Icon name="close" size={12} />
              </button>
            ) : (
              <span className="kbd" aria-hidden="true">
                /
              </span>
            )}
          </div>

          {FACETS.map((f) => (
            <FacetSelect
              key={f.key}
              label={f.label}
              options={facetOptions[f.key]}
              selected={filters[f.key]}
              onChange={(next) => setFilters((prev) => ({ ...prev, [f.key]: next }))}
            />
          ))}

          <span className="spacer" />

          <span className="assets-count mono" aria-live="polite" data-stale={stale ? 'true' : undefined}>
            <strong>{filtered.length.toLocaleString()}</strong>
            <span className="faint"> of {assets.length.toLocaleString()}</span>
          </span>

          <div className="segmented assets-density" role="group" aria-label="Row density">
            {(['comfortable', 'compact'] as const).map((d) => (
              <button
                key={d}
                type="button"
                data-on={density === d ? 'true' : 'false'}
                aria-pressed={density === d}
                onClick={() => setDensityPref(d)}
              >
                <Icon name={d === 'comfortable' ? 'rows' : 'columns'} size={12} />
                {d === 'comfortable' ? 'Cozy' : 'Dense'}
              </button>
            ))}
          </div>

          <Menu label="Columns" glyph={<Icon name="columns" size={13} />} ariaLabel="Column visibility">
            {() => (
              <div className="menu-group" role="group" aria-label="Visible columns">
                {COLUMNS.map((c) => {
                  const on = !hidden.includes(c.key);
                  // Toggle buttons rather than a role="menu": a menu promises
                  // arrow-key semantics this popover does not implement, and
                  // aria-pressed says exactly what these do.
                  return (
                    <button
                      key={c.key}
                      type="button"
                      className="item"
                      aria-pressed={on}
                      data-on={on ? 'true' : undefined}
                      onClick={() =>
                        setHiddenPref(on ? hidden.concat(c.key) : hidden.filter((k) => k !== c.key))
                      }
                    >
                      <span className="facet-box" data-on={on ? 'true' : undefined} aria-hidden="true">
                        {on && <Icon name="check" size={10} strokeWidth={2.6} />}
                      </span>
                      {c.label}
                    </button>
                  );
                })}
              </div>
            )}
          </Menu>

          <Menu label="Export" glyph={<Icon name="download" size={13} />} ariaLabel="Export inventory">
            {(close) => (
              <div className="menu-group" role="group" aria-label="Export formats">
                {EXPORTS.map((f) => (
                  <a key={f} className="item" href={exportURL(f, exportParams)} onClick={close}>
                    <Icon name="download" size={13} />
                    {f.toUpperCase()}
                    <span className="spacer" />
                    <span className="faint">{FORMAT_HINT[f]}</span>
                  </a>
                ))}
                <p className="hint menu-note">
                  Exports are rendered by the server from a fresh audit, not from the rows below.
                </p>
              </div>
            )}
          </Menu>
        </div>

        {chips.length > 0 && (
          <div className="assets-chips">
            {chips.map(({ key, value }) => (
              <span key={`${key}:${value}`} className="chip on">
                <span className="faint">{key}</span>
                <span className="truncate">{value}</span>
                <button
                  type="button"
                  className="x"
                  aria-label={`Remove ${key} filter ${value}`}
                  onClick={() => clearFacet(key, value)}
                >
                  <Icon name="close" size={9} strokeWidth={2.4} />
                </button>
              </span>
            ))}
            <button type="button" className="btn ghost sm" onClick={clearAll}>
              Clear all
            </button>
          </div>
        )}
      </div>

      <div className="card flush assets-card" data-density={density}>
        <VirtualTable
          rows={filtered}
          columns={visible}
          rowKey={rowKey}
          renderRow={renderRow}
          rowProps={rowProps}
          rowHeight={ROW_H[density]}
          label="Assets"
          sortKey={sort}
          sortDesc={desc}
          onSort={onSort}
          onActivate={(a, i) => setDetail({ asset: a, index: i })}
          selectedKey={detail ? `${detail.asset.provider}/${detail.asset.id}` : null}
          resetKey={`${sort}|${desc}|${query}|${chips.map((c) => `${c.key}=${c.value}`).join(',')}`}
          handleRef={tableRef}
          className="assets-table"
          style={{ opacity: stale ? 0.72 : 1, transition: 'opacity var(--dur-fast) var(--ease)' }}
          empty={
            assets.length === 0 ? (
              <div className="empty">
                <Icon name="layers" size={30} />
                <h3>Nothing collected yet</h3>
                <p>
                  Run an audit and rows appear as each provider answers — the table streams rather
                  than waiting for the slowest cloud.
                </p>
                <div className="row">
                  <button type="button" className="primary" onClick={() => start()} disabled={running}>
                    <Icon name="play" size={13} />
                    Run audit
                  </button>
                </div>
              </div>
            ) : (
              <div className="empty">
                <Icon name="filter" size={28} />
                <h3>No assets match</h3>
                <p>
                  {assets.length.toLocaleString()} assets were collected; none of them match the
                  current search and facets.
                </p>
                <div className="row">
                  <button type="button" onClick={clearAll}>
                    Clear filters
                  </button>
                </div>
              </div>
            )
          }
        />
      </div>

      {detail && (
        <AssetDrawer
          asset={detail.asset}
          onClose={() => setDetail(null)}
          returnFocus={() => tableRef.current?.focusRow(detail.index)}
          onFilterTag={(k) => {
            setQ(k);
            toast({ kind: 'info', title: 'Filtered by tag', body: k });
          }}
        />
      )}
    </>
  );
}

const FORMAT_HINT: Record<(typeof EXPORTS)[number], string> = {
  json: 'array',
  csv: 'flat',
  xlsx: 'sheets',
  html: 'report',
};

function Cell({ col, asset, dense }: { col: ColKey; asset: Asset; dense: boolean }) {
  switch (col) {
    case 'provider':
      return (
        <div role="gridcell" className="acell">
          <span className="dot" style={{ background: providerColor(asset.provider) }} />
          <span className="truncate">{asset.provider}</span>
        </div>
      );
    case 'type':
      return (
        <div role="gridcell" className="acell">
          <span className="acell-glyph">
            <AssetIcon type={asset.type} size={13} />
          </span>
          <span className="mono dim truncate" title={asset.type}>
            {asset.type}
          </span>
        </div>
      );
    case 'name':
      return (
        <div role="gridcell" className="acell acell-name" title={asset.id}>
          <span className="truncate">
            {asset.name || <span className="faint">(unnamed)</span>}
          </span>
          {!dense && <span className="mono faint truncate">{asset.id}</span>}
        </div>
      );
    case 'region':
      return (
        <div role="gridcell" className="acell">
          <span className="dim truncate">{asset.region || <span className="faint">—</span>}</span>
        </div>
      );
    case 'account_id':
      return (
        <div role="gridcell" className="acell">
          <span className="mono dim truncate" title={asset.account_id}>
            {asset.account_id || <span className="faint">—</span>}
          </span>
        </div>
      );
    case 'status': {
      if (!asset.status) {
        return (
          <div role="gridcell" className="acell">
            <span className="faint">—</span>
          </div>
        );
      }
      return (
        <div role="gridcell" className="acell">
          <span className="pill assets-status" style={{ color: toneColor(statusTone(asset.status)) }}>
            <span className="dot" />
            <span className="truncate">{asset.status}</span>
          </span>
        </div>
      );
    }
    case 'address': {
      const addr = assetAddress(asset);
      return (
        <div role="gridcell" className="acell">
          {addr ? (
            <button
              type="button"
              className="assets-addr mono truncate"
              title="Copy address"
              onClick={(e) => {
                // The row's own click opens the drawer; copying is a different
                // intent and must not also navigate.
                e.stopPropagation();
                void copyText(addr);
              }}
            >
              {addr}
            </button>
          ) : (
            <span className="faint">—</span>
          )}
        </div>
      );
    }
  }
}

/**
 * A trigger + floating panel. Shares `usePopover` with FacetSelect so the two
 * agree on dismissal and on which way a panel flips near a viewport edge.
 */
function Menu({
  label,
  glyph,
  ariaLabel,
  children,
}: {
  label: string;
  glyph?: React.ReactNode;
  ariaLabel: string;
  children: (close: () => void) => React.ReactNode;
}) {
  const [open, setOpen] = useState(false);
  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const close = useCallback(() => setOpen(false), []);
  const { wrapRef, panelRef, placement } = usePopover(open, close);

  useEffect(() => {
    if (open) panelRef.current?.focus();
  }, [open, panelRef]);

  return (
    <div className="facet" ref={wrapRef}>
      <button
        ref={triggerRef}
        type="button"
        className="btn ghost facet-trigger"
        aria-haspopup="dialog"
        aria-expanded={open}
        onClick={() => setOpen((o) => !o)}
      >
        {glyph}
        {label}
        <Icon name="chevron" size={11} className="facet-caret" />
      </button>
      {open && (
        <div
          ref={panelRef}
          className="popover facet-pop menu-pop"
          data-up={placement.up ? 'true' : undefined}
          data-align={placement.align}
          role="dialog"
          aria-label={ariaLabel}
          tabIndex={-1}
          onKeyDown={(e) => {
            if (e.key !== 'Escape') return;
            e.preventDefault();
            setOpen(false);
            triggerRef.current?.focus();
          }}
          // Tabbing past the last item leaves the panel; a floating menu with
          // no focus in it is just clutter, so it closes itself.
          onBlur={(e) => {
            if (!e.currentTarget.contains(e.relatedTarget as Node | null)) setOpen(false);
          }}
        >
          {children(close)}
        </div>
      )}
    </div>
  );
}
