'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useAudit } from './AuditProvider';
import { providerColor } from '@/lib/colors';
import { vars } from '@/lib/css';
import { Icon } from '@/lib/icons';
import type { ProviderStat } from '@/lib/types';

const SCOPE_KEY = 'auditor-scope-open';

/** Module-scoped so the disclosure survives client-side navigation, which
 *  unmounts and remounts this component on every tab change. localStorage
 *  covers the reload on top of that. */
let scopeOpenCache: boolean | null = null;


/**
 * The run/stop bar shared by every page, so an audit can be kicked off from
 * wherever the user happens to be rather than only from one tab.
 *
 * Once a run has produced something the bar folds down to a single summary
 * row: after the first audit the pages below are what the operator is here to
 * read, and a permanently expanded scope picker costs them 120px of it.
 */
export function AuditControls() {
  const {
    providers,
    selectedProviders,
    setSelectedProviders,
    kubeContexts,
    selectedKubeContexts,
    setSelectedKubeContexts,
    running,
    assets,
    errors,
    initErrors,
    failure,
    elapsedMs,
    byProvider,
    start,
    stop,
    clear,
  } = useAudit();

  const [open, setOpen] = useState<boolean>(() => scopeOpenCache ?? true);

  useEffect(() => {
    if (scopeOpenCache !== null) return;
    try {
      const raw = window.localStorage.getItem(SCOPE_KEY);
      if (raw === 'true' || raw === 'false') {
        scopeOpenCache = raw === 'true';
        setOpen(scopeOpenCache);
      }
    } catch {
      // A themeless, preference-less session is fine; a crashed bar is not.
    }
  }, []);

  const setScopeOpen = useCallback((v: boolean) => {
    scopeOpenCache = v;
    setOpen(v);
    try {
      window.localStorage.setItem(SCOPE_KEY, String(v));
    } catch {
      // The choice just doesn't survive a reload.
    }
  }, []);

  // Collapse when a run ends rather than when it starts: while it streams, the
  // per-provider meters are the most informative thing on the page.
  const wasRunning = useRef(running);
  useEffect(() => {
    if (wasRunning.current && !running) setScopeOpen(false);
    wasRunning.current = running;
  }, [running, setScopeOpen]);

  // Timed locally, not from `startedAt`: that timestamp comes from the server's
  // clock, and a few seconds of skew behind a port-forward would render as a
  // negative elapsed time.
  const [liveMs, setLiveMs] = useState(0);
  useEffect(() => {
    if (!running) return;
    const from = Date.now();
    setLiveMs(0);
    const id = setInterval(() => setLiveMs(Date.now() - from), 250);
    return () => clearInterval(id);
  }, [running]);

  const errCount = errors.length + initErrors.length;
  const hasRun = elapsedMs != null || assets.length > 0 || failure != null;
  const collapsed = hasRun && !open;

  // An empty selection means "every registered provider", matching the API's
  // own default — so an untouched picker runs everything.
  const activeProviders = selectedProviders.length > 0 ? selectedProviders : providers;
  const isOn = (p: string) => selectedProviders.length === 0 || selectedProviders.includes(p);

  const toggleProvider = (p: string) => {
    const set = new Set(activeProviders);
    if (set.has(p)) set.delete(p);
    else set.add(p);
    const next = providers.filter((n) => set.has(n));
    // The API reads an empty list as "all registered", so there is no way to
    // say "none" — refuse the click that would empty the picker rather than
    // silently turning everything back on.
    if (next.length === 0) return;
    setSelectedProviders(next.length === providers.length ? [] : next);
  };

  const allContexts = kubeContexts.length > 0 && selectedKubeContexts.length === kubeContexts.length;

  const shares = useMemo(() => {
    const rows: Array<[string, ProviderStat]> = [];
    let total = 0;
    for (const [name, s] of byProvider) {
      if (s.count === 0) continue;
      rows.push([name, s]);
      total += s.count;
    }
    rows.sort((a, b) => b[1].count - a[1].count || a[0].localeCompare(b[0]));
    return { rows, total };
  }, [byProvider]);

  const readout = (
    <div className="row" style={{ gap: 8, flexWrap: 'nowrap' }}>
      {running && (
        <span className="pill live">
          <span className="pulse-dot" />
          streaming
        </span>
      )}
      <span className="mono" style={{ fontSize: 'var(--fs-md)' }}>
        <span className="tick" data-live={running || undefined}>
          {assets.length.toLocaleString()}
        </span>
        <span className="faint" style={{ marginLeft: 5 }}>
          assets
        </span>
      </span>
      {(running || elapsedMs != null) && (
        <span className="mono dim">{((running ? liveMs : (elapsedMs ?? 0)) / 1000).toFixed(1)}s</span>
      )}
      {errCount > 0 && (
        <span className="pill err">
          {errCount} error{errCount === 1 ? '' : 's'}
        </span>
      )}
      {failure && <span className="pill err">stream failed</span>}
    </div>
  );

  const actions = (
    <div className="row" style={{ gap: 8, flexWrap: 'nowrap' }}>
      {running ? (
        <button className="danger" onClick={stop} type="button">
          <Spinner />
          Stop
        </button>
      ) : (
        <button className="primary" onClick={() => start()} type="button">
          <Icon name="play" size={13} />
          Run audit
        </button>
      )}
      <button
        onClick={clear}
        disabled={running || (assets.length === 0 && errCount === 0 && failure == null)}
        type="button"
      >
        Clear
      </button>
    </div>
  );

  const disclosure = hasRun && (
    <button
      className="btn ghost sm"
      onClick={() => setScopeOpen(!open)}
      aria-expanded={open}
      aria-controls={open ? 'audit-scope' : undefined}
      type="button"
    >
      {/* The rotation lives on a wrapper because Icon exposes className, not
          style; an inline transition is still governed by the reduced-motion
          override in globals.css. */}
      <span
        style={{
          display: 'inline-flex',
          transform: open ? 'rotate(90deg)' : 'none',
          transition: 'transform var(--dur-fast) var(--ease)',
        }}
      >
        <Icon name="chevron" size={13} />
      </span>
      Scope
    </button>
  );

  return (
    <div className="card" style={{ marginBottom: 16 }}>
      <div
        className="card-body"
        style={{
          display: 'flex',
          flexDirection: 'column',
          gap: collapsed ? 0 : 12,
          padding: collapsed ? '9px 14px' : undefined,
        }}
      >
        {collapsed ? (
          <div className="row" style={{ gap: 10 }}>
            {disclosure}
            <ScopeSummary
              providers={activeProviders}
              allProviders={selectedProviders.length === 0}
              contexts={selectedKubeContexts}
            />
            <span className="spacer" />
            {readout}
            {actions}
          </div>
        ) : (
          <>
            <div id="audit-scope" className="row" style={{ gap: 8 }}>
              <Label>Providers</Label>
              {providers.length === 0 && <span className="faint">none registered</span>}
              {providers.map((p) => {
                const on = isOn(p);
                const n = byProvider.get(p)?.count ?? 0;
                return (
                  <button
                    key={p}
                    className={`chip${on ? ' on' : ''}`}
                    style={on ? { borderColor: providerColor(p), color: providerColor(p) } : undefined}
                    onClick={() => toggleProvider(p)}
                    disabled={running}
                    aria-pressed={on}
                    type="button"
                  >
                    <span className="dot" style={{ background: providerColor(p) }} />
                    {p}
                    {n > 0 && <span className="count-badge">{n.toLocaleString()}</span>}
                  </button>
                );
              })}
              {selectedProviders.length > 0 && (
                <button
                  className="btn ghost sm"
                  onClick={() => setSelectedProviders([])}
                  disabled={running}
                  type="button"
                >
                  All
                </button>
              )}
              {hasRun && (
                <>
                  <span className="spacer" />
                  {disclosure}
                </>
              )}
            </div>

            {kubeContexts.length > 0 && (
              <div className="row" style={{ gap: 8 }}>
                <Label>Clusters</Label>
                {kubeContexts.map((c) => (
                  <button
                    key={c}
                    className={`chip${selectedKubeContexts.includes(c) ? ' on' : ''}`}
                    onClick={() =>
                      setSelectedKubeContexts(
                        selectedKubeContexts.includes(c)
                          ? selectedKubeContexts.filter((n) => n !== c)
                          : selectedKubeContexts.concat(c),
                      )
                    }
                    disabled={running}
                    aria-pressed={selectedKubeContexts.includes(c)}
                    type="button"
                  >
                    {c}
                  </button>
                ))}
                <button
                  className="btn ghost sm"
                  onClick={() => setSelectedKubeContexts(allContexts ? [] : kubeContexts)}
                  disabled={running}
                  type="button"
                >
                  {allContexts ? 'None' : 'All'}
                </button>
                {selectedKubeContexts.length === 0 && (
                  <span className="hint">none picked → the server&apos;s current context</span>
                )}
              </div>
            )}

            {running && shares.rows.length > 0 && (
              <div style={{ display: 'flex', flexDirection: 'column', gap: 5 }}>
                {shares.rows.map(([name, s]) => {
                  const pct = shares.total > 0 ? (s.count / shares.total) * 100 : 0;
                  return (
                    <div key={name} className="row" style={{ gap: 10, flexWrap: 'nowrap' }}>
                      <span className="dot" style={{ background: providerColor(name) }} />
                      <span className="mono truncate" style={{ width: 108, color: 'var(--text-dim)' }}>
                        {name}
                      </span>
                      <span className="meter">
                        <span
                          className="meter-fill"
                          style={vars({ '--meter-color': providerColor(name) }, { width: `${pct}%` })}
                        />
                      </span>
                      <span className="mono" style={{ minWidth: 62, textAlign: 'right' }}>
                        {s.count.toLocaleString()}
                      </span>
                      <span className="mono faint" style={{ minWidth: 38, textAlign: 'right' }}>
                        {pct.toFixed(0)}%
                      </span>
                    </div>
                  );
                })}
              </div>
            )}

            <div className="row" style={{ gap: 10 }}>
              {actions}
              <span className="spacer" />
              {readout}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function Label({ children }: { children: React.ReactNode }) {
  return (
    <strong
      className="dim"
      style={{ fontSize: 'var(--fs-xs)', letterSpacing: '0.04em', textTransform: 'uppercase' }}
    >
      {children}
    </strong>
  );
}

/** The one-line "what will run" line shown while the picker is folded away. */
function ScopeSummary({
  providers,
  allProviders,
  contexts,
}: {
  providers: string[];
  allProviders: boolean;
  contexts: string[];
}) {
  const shown = providers.slice(0, 7);
  return (
    <span className="row" style={{ gap: 6, flexWrap: 'nowrap', minWidth: 0 }}>
      <span className="row" style={{ gap: 3, flexWrap: 'nowrap' }}>
        {shown.map((p) => (
          <span key={p} className="dot" style={{ background: providerColor(p) }} title={p} />
        ))}
      </span>
      <span className="dim truncate">
        {providers.length === 0
          ? 'no providers'
          : allProviders
            ? `all ${providers.length} providers`
            : providers.length === 1
              ? providers[0]
              : `${providers.length} providers`}
        {contexts.length > 0 && ` · ${contexts.length} cluster${contexts.length === 1 ? '' : 's'}`}
      </span>
    </span>
  );
}

/**
 * Marching-ants ring rather than a rotating arc: rotation would need a
 * @keyframes of its own, and an SVG <animateTransform> is invisible to the
 * `prefers-reduced-motion` override in globals.css. `dash-march` is already
 * defined there, so this stops moving when the operator asks it to.
 * r = 60/2π, so the 6+4 dash pattern divides the circumference exactly.
 */
function Spinner() {
  return (
    <svg width="13" height="13" viewBox="0 0 24 24" aria-hidden="true" style={{ display: 'block' }}>
      <circle cx="12" cy="12" r="9.55" fill="none" stroke="currentColor" strokeOpacity="0.25" strokeWidth="2.5" />
      <circle
        cx="12"
        cy="12"
        r="9.55"
        fill="none"
        stroke="currentColor"
        strokeWidth="2.5"
        strokeLinecap="round"
        strokeDasharray="6 4"
        style={{ animation: 'dash-march 0.55s linear infinite' }}
      />
    </svg>
  );
}
