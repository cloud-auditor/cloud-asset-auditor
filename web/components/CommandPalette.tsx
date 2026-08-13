'use client';

import { Fragment, useEffect, useMemo, useRef, useState } from 'react';
import { useRouter } from 'next/navigation';
import { useAudit } from './AuditProvider';
import { exportURL, topologyDownloadURL } from '@/lib/api';
import { providerColor } from '@/lib/colors';
import { match, type FuzzyMatch } from '@/lib/fuzzy';
import { useTheme, type Theme } from '@/lib/theme';
import { fmtCount } from '@/lib/format';

const SECTIONS = ['Navigate', 'Audit', 'Export', 'Appearance', 'Assets'] as const;
type Section = (typeof SECTIONS)[number];

interface Item {
  id: string;
  section: Section;
  label: string;
  hint?: string;
  /** Present on toggles; rendered as an on/off chip. */
  on?: boolean;
  /** Provider dot colour, for the provider toggles and asset hits. */
  color?: string;
  run: () => void;
}

type Result = Item & { ranges: [number, number][]; score: number };

const AUDIT_FORMATS = ['json', 'csv', 'xlsx', 'html'] as const;
const TOPOLOGY_FORMATS = ['dot', 'mermaid', 'd2', 'graphml', 'excalidraw', 'drawio', 'html'] as const;
const THEMES: { value: Theme; label: string }[] = [
  { value: 'light', label: 'Light' },
  { value: 'dark', label: 'Dark' },
  { value: 'system', label: 'System' },
];

/** Assets are only searched once the query is specific enough to be worth a
 *  full scan of an inventory that can hold 50k rows. */
const ASSET_MIN_QUERY = 2;
const ASSET_LIMIT = 8;

/**
 * Navigates to an asset on the Assets page.
 *
 * The push alone is not enough. When the palette is opened *from* the Assets
 * page the push is same-route: the URL changes, the page does not remount, and
 * its one-shot `?q=` read already ran — so picking an asset would silently do
 * nothing. The page also listens for this event; announcing the query is what
 * makes the two entry points behave the same.
 */
const ASSETS_QUERY_EVENT = 'auditor:assets-query';

function gotoAsset(router: { push: (href: string) => void }, id: string): void {
  router.push(`/assets/?q=${encodeURIComponent(id)}`);
  window.dispatchEvent(new CustomEvent(ASSETS_QUERY_EVENT, { detail: id }));
}

export function CommandPalette() {
  const router = useRouter();
  const { theme, setTheme } = useTheme();
  const {
    assets,
    running,
    providers,
    selectedProviders,
    setSelectedProviders,
    kubeContexts,
    selectedKubeContexts,
    setSelectedKubeContexts,
    start,
    stop,
    clear,
  } = useAudit();

  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);

  const inputRef = useRef<HTMLInputElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);

  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && !e.altKey && e.key.toLowerCase() === 'k') {
        e.preventDefault();
        setOpen((o) => !o);
      }
    };
    const onRequest = () => setOpen(true);
    window.addEventListener('keydown', onKey);
    window.addEventListener('auditor:palette', onRequest);
    return () => {
      window.removeEventListener('keydown', onKey);
      window.removeEventListener('auditor:palette', onRequest);
    };
  }, []);

  // Focus the input on open and hand focus back to whatever had it on close —
  // usually the topbar's ⌘K button, which must not lose its place.
  useEffect(() => {
    if (!open) return;
    const prev = document.activeElement;
    inputRef.current?.focus();
    setQuery('');
    setActive(0);
    return () => {
      if (prev instanceof HTMLElement && prev.isConnected) prev.focus();
    };
  }, [open]);

  const items = useMemo<Item[]>(() => {
    const out: Item[] = [];
    const go = (href: string, label: string) =>
      out.push({ id: `nav:${href}`, section: 'Navigate', label, hint: href, run: () => router.push(href) });

    go('/', 'Dashboard');
    go('/assets/', 'Assets');
    go('/topology/', 'Topology');
    go('/exposure/', 'Exposure');

    if (running) out.push({ id: 'audit:stop', section: 'Audit', label: 'Stop audit', run: stop });
    else out.push({ id: 'audit:start', section: 'Audit', label: 'Run audit', run: start });
    if (assets.length > 0) {
      out.push({
        id: 'audit:clear',
        section: 'Audit',
        label: 'Clear results',
        hint: `${fmtCount(assets.length)} assets`,
        run: clear,
      });
    }

    for (const p of providers) {
      // An empty selection means "all", so switching one off has to materialise
      // the complement — collapsing to `[p]` would silently turn the other
      // providers off, which is the opposite of what the label promises.
      const all = selectedProviders.length === 0;
      const current = all ? providers : selectedProviders;
      const on = current.includes(p);
      out.push({
        id: `provider:${p}`,
        section: 'Audit',
        label: `Provider: ${p}`,
        color: providerColor(p),
        on,
        run: () => {
          const next = on ? current.filter((x) => x !== p) : current.concat(p);
          setSelectedProviders(next.length === providers.length ? [] : next);
        },
      });
    }

    for (const c of kubeContexts) {
      const on = selectedKubeContexts.includes(c);
      out.push({
        id: `kube:${c}`,
        section: 'Audit',
        label: `Cluster: ${c}`,
        on,
        run: () =>
          setSelectedKubeContexts(
            on ? selectedKubeContexts.filter((x) => x !== c) : selectedKubeContexts.concat(c),
          ),
      });
    }

    const auditParams = { providers: selectedProviders, kubeContexts: selectedKubeContexts };
    for (const f of AUDIT_FORMATS) {
      out.push({
        id: `export:audit:${f}`,
        section: 'Export',
        label: `Download inventory as ${f.toUpperCase()}`,
        hint: 'assets',
        run: () => download(exportURL(f, auditParams)),
      });
    }
    for (const f of TOPOLOGY_FORMATS) {
      out.push({
        id: `export:topo:${f}`,
        section: 'Export',
        label: `Download topology as ${f}`,
        hint: 'graph',
        // No detail/group knobs: the palette is reachable from every page and
        // only the Topology page knows what is currently on screen.
        run: () => download(topologyDownloadURL(f, {})),
      });
    }

    for (const t of THEMES) {
      out.push({
        id: `theme:${t.value}`,
        section: 'Appearance',
        label: `Theme: ${t.label}`,
        on: theme === t.value,
        run: () => setTheme(t.value),
      });
    }

    return out;
  }, [
    router,
    running,
    assets.length,
    providers,
    selectedProviders,
    setSelectedProviders,
    kubeContexts,
    selectedKubeContexts,
    setSelectedKubeContexts,
    start,
    stop,
    clear,
    theme,
    setTheme,
  ]);

  const results = useMemo<Result[]>(() => {
    // The palette stays mounted when closed (it owns the ⌘K listener), and the
    // last query survives a close. Without this guard every 200ms stream flush
    // would re-run the asset scan below over an inventory of 50k rows for a
    // list nobody is looking at.
    if (!open) return [];

    const q = query.trim();
    const scored: Result[] = [];

    for (const it of items) {
      if (!q) {
        scored.push({ ...it, ranges: [], score: 0 });
        continue;
      }
      const m = match(q, it.label);
      if (m) {
        scored.push({ ...it, ranges: m.ranges, score: m.score });
        continue;
      }
      // Section-qualified fallback, so "export csv" finds a label that never
      // contains the word "export". The offsets belong to the joined string,
      // not the label, so the hit is scored but not highlighted.
      const viaSection = match(q, `${it.section} ${it.label}`);
      if (viaSection) scored.push({ ...it, ranges: [], score: viaSection.score - 8 });
    }

    if (q.length >= ASSET_MIN_QUERY) {
      // A linear pass over every asset, keeping only the top few. Sorting the
      // whole inventory per keystroke would be the expensive part, so the
      // shortlist is maintained by insertion instead.
      const top: Result[] = [];
      assets.forEach((a, i) => {
        let m: FuzzyMatch | null = match(q, a.name);
        let ranges = m?.ranges ?? [];
        if (!m) {
          m = match(q, a.id);
          ranges = [];
        }
        if (!m) return;
        if (top.length === ASSET_LIMIT && m.score <= top[top.length - 1].score) return;
        top.push({
          id: `asset:${i}`,
          section: 'Assets',
          label: a.name || a.id,
          hint: `${a.type}${a.region ? ` · ${a.region}` : ''}`,
          color: providerColor(a.provider),
          ranges,
          score: m.score,
          run: () => gotoAsset(router, a.id),
        });
        top.sort((x, y) => y.score - x.score);
        if (top.length > ASSET_LIMIT) top.length = ASSET_LIMIT;
      });
      scored.push(...top);
    }

    // Sections keep their authored order; within one, the best match leads.
    return scored.sort((a, b) => {
      const s = SECTIONS.indexOf(a.section) - SECTIONS.indexOf(b.section);
      if (s !== 0) return s;
      return q ? b.score - a.score : 0;
    });
  }, [open, items, assets, query, router]);

  const activeIdx = results.length === 0 ? -1 : Math.min(active, results.length - 1);

  useEffect(() => {
    if (activeIdx < 0) return;
    listRef.current?.querySelector(`#palette-opt-${activeIdx}`)?.scrollIntoView({ block: 'nearest' });
  }, [activeIdx]);

  if (!open) return null;

  const move = (delta: number) =>
    setActive((i) => {
      const n = results.length;
      if (n === 0) return 0;
      return (Math.min(i, n - 1) + delta + n) % n;
    });

  const onKeyDown = (e: React.KeyboardEvent) => {
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        setOpen(false);
        break;
      case 'Tab':
        // The dialog has exactly one focusable control by design — the results
        // are a listbox driven by aria-activedescendant, not a tab ring — so
        // trapping is just refusing to leave the input.
        e.preventDefault();
        break;
      case 'ArrowDown':
        e.preventDefault();
        move(1);
        break;
      case 'ArrowUp':
        e.preventDefault();
        move(-1);
        break;
      case 'Home':
        e.preventDefault();
        setActive(0);
        break;
      case 'End':
        e.preventDefault();
        setActive(Math.max(0, results.length - 1));
        break;
      case 'Enter': {
        e.preventDefault();
        const hit = results[activeIdx];
        if (!hit) break;
        setOpen(false);
        hit.run();
        break;
      }
    }
  };

  return (
    <div
      className="modal-backdrop"
      onMouseDown={(e) => {
        if (e.target === e.currentTarget) setOpen(false);
      }}
    >
      <div className="modal" role="dialog" aria-modal="true" aria-label="Command palette" onKeyDown={onKeyDown}>
        <header>
          <SearchIcon />
          <input
            ref={inputRef}
            value={query}
            onChange={(e) => {
              setQuery(e.target.value);
              setActive(0);
            }}
            placeholder="Search actions, pages, and assets…"
            aria-label="Search commands"
            role="combobox"
            aria-expanded="true"
            aria-autocomplete="list"
            aria-controls="palette-list"
            aria-activedescendant={activeIdx >= 0 ? `palette-opt-${activeIdx}` : undefined}
            autoComplete="off"
            spellCheck={false}
          />
          <span className="kbd">Esc</span>
        </header>

        <div className="body" id="palette-list" role="listbox" aria-label="Results" ref={listRef}>
          {results.length === 0 && (
            <div className="empty" role="presentation" style={{ padding: '28px 12px' }}>
              <p>
                Nothing matches <strong className="mono">{query}</strong>.
              </p>
              <p className="hint">
                Try a page, an action, or part of an asset name. Assets become searchable once a run
                has streamed some in.
              </p>
            </div>
          )}
          {results.map((r, i) => (
            <Fragment key={r.id}>
              {/* role=presentation: a listbox may only contain options and
                  groups, and an unroled div between them makes the option
                  count read wrong in several screen readers. The heading is
                  visual grouping; the option labels already name themselves. */}
              {(i === 0 || results[i - 1].section !== r.section) && (
                <div className="sect" role="presentation">
                  {r.section}
                </div>
              )}
              <div
                id={`palette-opt-${i}`}
                role="option"
                aria-selected={i === activeIdx}
                className="item"
                data-active={i === activeIdx ? 'true' : undefined}
                onClick={() => {
                  setOpen(false);
                  r.run();
                }}
                onMouseMove={() => setActive(i)}
              >
                {r.color && <span className="dot" style={{ background: r.color }} />}
                <span className="truncate">
                  <Highlight text={r.label} ranges={r.ranges} />
                </span>
                <span className="spacer" />
                {r.on !== undefined && <span className={`chip${r.on ? ' on' : ''}`}>{r.on ? 'on' : 'off'}</span>}
                {r.hint && (
                  <span className="faint truncate" style={{ fontSize: 'var(--fs-xs)', maxWidth: 200 }}>
                    {r.hint}
                  </span>
                )}
              </div>
            </Fragment>
          ))}
        </div>

        <footer>
          <span className="kbd">↑</span>
          <span className="kbd">↓</span> move
          <span className="kbd">↵</span> run
          <span className="kbd">esc</span> close
          <span className="spacer" />
          <span>{results.length} result{results.length === 1 ? '' : 's'}</span>
        </footer>
      </div>
    </div>
  );
}

function Highlight({ text, ranges }: { text: string; ranges: [number, number][] }) {
  if (ranges.length === 0) return <>{text}</>;
  const out: React.ReactNode[] = [];
  let at = 0;
  for (const [s, e] of ranges) {
    if (s > at) out.push(text.slice(at, s));
    out.push(<mark key={s}>{text.slice(s, e)}</mark>);
    at = e;
  }
  if (at < text.length) out.push(text.slice(at));
  return <>{out}</>;
}

/** An anchor click rather than window.open: the export endpoints answer with a
 *  Content-Disposition attachment, so a real navigation downloads the file
 *  instead of leaving a blank tab behind. */
function download(url: string) {
  const a = document.createElement('a');
  a.href = url;
  a.target = '_blank';
  a.rel = 'noopener noreferrer';
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function SearchIcon() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" aria-hidden="true" style={{ flex: 'none' }}>
      <circle cx="10.5" cy="10.5" r="6.7" stroke="var(--text-faint)" strokeWidth="1.8" />
      <path d="M15.5 15.5 21 21" stroke="var(--text-faint)" strokeWidth="1.8" strokeLinecap="round" />
    </svg>
  );
}
