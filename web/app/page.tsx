'use client';

import { useCallback, useMemo, useState } from 'react';
import { useAudit } from '@/components/AuditProvider';
import { AuditControls } from '@/components/AuditControls';
import { BarList, type BarRow } from '@/components/BarList';
import { Donut, type DonutSegment } from '@/components/Donut';
import { Sparkline } from '@/components/Sparkline';
import { StatTile, StatTileSkeleton } from '@/components/StatTile';
import { PROVIDERS_WITH_COLOR, providerColor } from '@/lib/colors';
import { vars } from '@/lib/css';
import { Icon } from '@/lib/icons';
import type { ProviderStat } from '@/lib/types';
import './dashboard.css';
import { fmtCount } from '@/lib/format';


/** What one provider contributed, kept per provider so cross-filtering the
 *  ranked lists is a map lookup rather than a second pass over 50k assets. */
interface ProviderAgg {
  count: number;
  types: Map<string, number>;
  regions: Map<string, number>;
  accounts: Set<string>;
}

function rank(m: Map<string, number>, limit: number): BarRow[] {
  return [...m.entries()]
    // Ties break on the label so a live stream cannot reorder equal rows on
    // every 200ms flush — a list that reshuffles while you read it is unusable.
    .sort((a, b) => b[1] - a[1] || a[0].localeCompare(b[0]))
    .slice(0, limit)
    .map(([label, value]) => ({ label, value }));
}

function formatSpan(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  return `${Math.floor(ms / 60_000)}m ${String(Math.round((ms % 60_000) / 1000)).padStart(2, '0')}s`;
}

/**
 * Buckets an error line under the provider that raised it.
 *
 * Mirrors `attributeError` in AuditProvider, which is module-private there:
 * the server sends `{"message": …}` with no structural provider field, so the
 * name has to be read back out of the text. Anything unrecognised is grouped as
 * unattributed rather than guessed at.
 */
function errorGroup(msg: string, known: readonly string[]): string {
  const quoted = /^provider "([^"]+)"/.exec(msg);
  if (quoted) return quoted[1];
  const head = msg.split(/[\s:]/, 1)[0]?.toLowerCase() ?? '';
  return head !== '' && known.includes(head) ? head : 'unattributed';
}

/**
 * Per-provider state, without a timer.
 *
 * While a run is live every contributing provider reads "collecting": the SSE
 * stream carries no per-provider completion event, and inferring one from a
 * stale `lastAt` would flip a slow provider to "complete" halfway through its
 * collection. Vague is better than wrong here.
 */
function healthStatus(s: ProviderStat, running: boolean): { cls: string; text: string } {
  if (s.errors > 0 && s.count === 0) return { cls: 'err', text: 'failed' };
  if (s.errors > 0) return { cls: 'warn', text: 'partial' };
  if (running) return { cls: 'live', text: 'collecting' };
  if (s.count > 0) return { cls: 'ok', text: 'complete' };
  return { cls: '', text: 'idle' };
}

export default function DashboardPage() {
  const {
    assets,
    arrival,
    byProvider,
    errors,
    initErrors,
    failure,
    running,
    elapsedMs,
    providers,
    start,
    toast,
  } = useAudit();

  const [focus, setFocus] = useState<string | null>(null);
  const [errorsOpen, setErrorsOpen] = useState(false);

  const agg = useMemo(() => {
    const perProvider = new Map<string, ProviderAgg>();
    const types = new Map<string, number>();
    const regions = new Map<string, number>();
    const accounts = new Set<string>();
    let unattributed = 0;

    for (const a of assets) {
      let p = perProvider.get(a.provider);
      if (!p) {
        p = { count: 0, types: new Map(), regions: new Map(), accounts: new Set() };
        perProvider.set(a.provider, p);
      }
      p.count += 1;
      p.types.set(a.type, (p.types.get(a.type) ?? 0) + 1);
      types.set(a.type, (types.get(a.type) ?? 0) + 1);
      if (a.region) {
        p.regions.set(a.region, (p.regions.get(a.region) ?? 0) + 1);
        regions.set(a.region, (regions.get(a.region) ?? 0) + 1);
      }
      if (a.account_id) {
        // Account ids are only unique within a provider — two clouds may both
        // call an account "default".
        const key = `${a.provider}/${a.account_id}`;
        p.accounts.add(key);
        accounts.add(key);
      } else {
        unattributed += 1;
      }
    }
    return { perProvider, types, regions, accounts, unattributed };
  }, [assets]);

  // Derived, not stored: a re-run with a narrower scope drops providers, and a
  // filter chip naming one that no longer exists would filter to nothing with
  // no way to tell why. Deriving also avoids an effect that writes state.
  const activeFocus = focus !== null && agg.perProvider.has(focus) ? focus : null;
  const scoped = activeFocus !== null ? (agg.perProvider.get(activeFocus) ?? null) : null;

  const typeRows = useMemo(
    () => rank(scoped ? scoped.types : agg.types, 12),
    [scoped, agg.types],
  );
  const regionRows = useMemo(
    () => rank(scoped ? scoped.regions : agg.regions, 12),
    [scoped, agg.regions],
  );
  const scopedTotal = scoped ? scoped.count : assets.length;

  // Read off the unfiltered maps on purpose: these annotate stat tiles whose
  // values are inventory-wide, and a sub-line that quietly followed the
  // cross-filter while the number above it did not would be a lie.
  const topType = useMemo(() => {
    const r = rank(agg.types, 1);
    return r.length > 0 ? r[0].label : undefined;
  }, [agg.types]);
  const topRegion = useMemo(() => {
    const r = rank(agg.regions, 1);
    return r.length > 0 ? r[0].label : undefined;
  }, [agg.regions]);

  const donutSegments = useMemo<DonutSegment[]>(
    () =>
      [...agg.perProvider.entries()]
        .map(([key, v]) => ({ key, label: key, value: v.count, color: providerColor(key) }))
        .sort((a, b) => b.value - a.value),
    [agg.perProvider],
  );

  const health = useMemo(() => {
    const rows: Array<{ name: string; s: ProviderStat }> = [];
    for (const [name, s] of byProvider) {
      // '' is the bucket for errors no provider could be read out of; it is not
      // a provider and has no collection health. The errors panel shows it.
      if (name === '') continue;
      rows.push({ name, s });
    }
    rows.sort((a, b) => b.s.count - a.s.count || a.name.localeCompare(b.name));
    return rows;
  }, [byProvider]);

  const known = useMemo(
    () => Array.from(new Set([...PROVIDERS_WITH_COLOR, ...providers])),
    [providers],
  );

  const errorGroups = useMemo(() => {
    const groups = new Map<string, Array<{ msg: string; init: boolean }>>();
    const push = (msg: string, init: boolean) => {
      const key = errorGroup(msg, known);
      const list = groups.get(key);
      if (list) list.push({ msg, init });
      else groups.set(key, [{ msg, init }]);
    };
    for (const e of initErrors) push(e, true);
    for (const e of errors) push(e, false);
    return [...groups.entries()].sort(
      (a, b) => b[1].length - a[1].length || a[0].localeCompare(b[0]),
    );
  }, [errors, initErrors, known]);

  const errorCount = errors.length + initErrors.length;

  const copyErrors = useCallback(async () => {
    const lines = errorGroups.flatMap(([, list]) => list.map((e) => e.msg));
    try {
      // The server is routinely reached over plain http on a LAN address or a
      // port-forward, where the clipboard API is absent rather than merely
      // denied — so this is a normal outcome, not an exception path.
      if (!navigator.clipboard) throw new Error('unavailable');
      await navigator.clipboard.writeText(lines.join('\n'));
      toast({
        kind: 'ok',
        title: 'Copied',
        body: `${lines.length} error line${lines.length === 1 ? '' : 's'}`,
      });
    } catch {
      toast({
        kind: 'err',
        title: 'Copy failed',
        body: 'The clipboard is unavailable on this origin — select the lines instead.',
      });
    }
  }, [errorGroups, toast]);

  const arrivalValues = useMemo(() => arrival.map((b) => b.n), [arrival]);
  const peak = arrivalValues.length > 0 ? Math.max(...arrivalValues) : 0;
  const lastSecond = arrivalValues.length > 0 ? arrivalValues[arrivalValues.length - 1] : 0;

  const hasDemo = providers.includes('demo');
  const showSkeleton = running && assets.length === 0;
  const showEmpty = !running && assets.length === 0 && errorCount === 0 && failure === null;

  return (
    <div className="dash stagger">
      <div className="page-head">
        <div>
          <h1>Dashboard</h1>
          <p>Inventory shape across every configured provider.</p>
        </div>
      </div>

      {/* AuditControls carries its own 16px bottom margin as an inline style,
          which this column's gap would double. Cancelled here rather than by
          reaching into a component another surface also renders. */}
      <div className="dash-controls">
        <AuditControls />
      </div>

      {failure && (
        <div className="banner error">
          <Icon name="alert" size={15} />
          <span>
            Audit stream failed: <span className="mono">{failure}</span>
          </span>
        </div>
      )}

      {showEmpty ? (
        <div className="card">
          <div className="empty">
            <Icon name="layers" size={30} strokeWidth={1.4} />
            <h3>No inventory yet</h3>
            <p>
              An audit asks every configured provider for everything it can see — Cloudflare zones
              and rulesets, OCI compartments across each subscribed region, every object type your
              Kubernetes clusters serve — and streams the results into this tab as they arrive.
            </p>
            <p className="hint">
              Nothing is written to disk. The snapshot lives in this page until you clear it or
              reload.
            </p>
            <div className="row">
              <button className="primary" type="button" onClick={() => start()}>
                <Icon name="play" size={13} />
                Run audit
              </button>
              {hasDemo && (
                <button type="button" onClick={() => start({ providers: ['demo'] })}>
                  Load demo data
                </button>
              )}
            </div>
            {providers.length > 0 && (
              <p className="hint">
                {providers.length} provider{providers.length === 1 ? '' : 's'} registered:{' '}
                <span className="mono">{providers.join(', ')}</span>
              </p>
            )}
          </div>
        </div>
      ) : showSkeleton ? (
        <>
          <div className="stats">
            {[0, 1, 2, 3, 4, 5].map((i) => (
              <StatTileSkeleton key={i} />
            ))}
          </div>
          <div className="dash-band">
            <div className="card">
              <div className="card-head">By provider</div>
              <div className="card-body dash-skel-block">
                <span className="skeleton dash-skel-ring" />
              </div>
            </div>
            <div className="card">
              <div className="card-head">Collection health</div>
              <div className="card-body dash-skel-rows">
                {[0, 1, 2, 3].map((i) => (
                  <span key={i} className="skeleton dash-skel-row" />
                ))}
              </div>
            </div>
          </div>
        </>
      ) : (
        <>
          <div className="stats">
            <StatTile
              label="Assets"
              value={assets.length}
              icon="layers"
              live={running}
              sub={
                running
                  ? `${fmtCount(lastSecond)}/s`
                  : elapsedMs != null
                    ? `in ${formatSpan(elapsedMs)}`
                    : undefined
              }
              spark={
                arrivalValues.length > 1 ? (
                  <Sparkline
                    values={arrivalValues}
                    label={`Arrival rate over the last ${arrivalValues.length} seconds, peaking at ${fmtCount(peak)} assets per second.`}
                  />
                ) : undefined
              }
            />
            <StatTile
              label="Providers"
              value={agg.perProvider.size}
              icon="cloud"
              sub={providers.length > 0 ? `of ${providers.length} registered` : undefined}
            />
            <StatTile
              label="Resource types"
              value={agg.types.size}
              icon="shapes"
              sub={topType}
              title={topType ? `Most common: ${topType}` : undefined}
            />
            <StatTile
              label="Accounts"
              value={agg.accounts.size}
              icon="building"
              sub={
                agg.unattributed > 0
                  ? `${fmtCount(agg.unattributed)} unattributed`
                  : undefined
              }
              title="Distinct account, tenancy, or cluster identifiers, counted per provider."
            />
            <StatTile
              label="Regions"
              value={agg.regions.size}
              icon="globe"
              sub={topRegion}
              title={topRegion ? `Most populated: ${topRegion}` : undefined}
            />
            <StatTile
              label="Errors"
              value={errorCount}
              icon="alert"
              color={errorCount > 0 ? 'var(--danger)' : undefined}
              sub={
                initErrors.length > 0
                  ? `${initErrors.length} provider${initErrors.length === 1 ? '' : 's'} skipped`
                  : errorCount > 0
                    ? 'results are partial'
                    : 'no failures'
              }
            />
          </div>

          <div className="dash-band">
            <div className="card">
              <div className="card-head">
                By provider
                <span className="spacer" />
                {activeFocus && (
                  <button className="btn ghost sm" type="button" onClick={() => setFocus(null)}>
                    Clear
                  </button>
                )}
              </div>
              <div className="card-body">
                <Donut
                  segments={donutSegments}
                  selected={activeFocus}
                  onSelect={setFocus}
                  unit="assets"
                />
              </div>
            </div>

            <div className="card">
              <div className="card-head">
                Collection health
                <span className="spacer" />
                <span className="hint">first → last arrival, measured in this tab</span>
              </div>
              <div className="card-body">
                {health.length === 0 ? (
                  <div className="faint">No provider reported yet.</div>
                ) : (
                  <div className="health">
                    <div className="health-row health-head" aria-hidden>
                      <span>Provider</span>
                      <span className="ar">Assets</span>
                      <span>Share</span>
                      <span className="ar">Duration</span>
                      <span className="ar">Errors</span>
                      <span>Status</span>
                    </div>
                    {health.map(({ name, s }) => {
                      const share = assets.length > 0 ? (s.count / assets.length) * 100 : 0;
                      const st = healthStatus(s, running);
                      const span = formatSpan(s.lastAt - s.firstAt);
                      return (
                        <button
                          key={name}
                          type="button"
                          className="health-row health-btn surface-rail"
                          style={vars({ '--rail': providerColor(name) })}
                          data-on={activeFocus === name ? 'true' : undefined}
                          aria-pressed={activeFocus === name}
                          aria-label={`${name}: ${fmtCount(s.count)} assets, ${share.toFixed(0)} percent of inventory, ${span}, ${s.errors} error${s.errors === 1 ? '' : 's'}, ${st.text}. Filter the lists below to ${name}.`}
                          onClick={() => setFocus(activeFocus === name ? null : name)}
                        >
                          <span className="health-name truncate">
                            <span className="dot" style={{ background: providerColor(name) }} />
                            {name}
                          </span>
                          <span className="mono ar">{fmtCount(s.count)}</span>
                          <span className="meter">
                            <span
                              className="meter-fill"
                              style={vars({ '--meter-color': providerColor(name) }, {
                                width: `${share}%`,
                              })}
                            />
                          </span>
                          <span className="mono dim ar">{span}</span>
                          <span className={`mono ar${s.errors > 0 ? ' health-err' : ' faint'}`}>
                            {s.errors}
                          </span>
                          <span className={`pill${st.cls ? ` ${st.cls}` : ''}`}>
                            {st.cls === 'live' && <span className="pulse-dot" />}
                            {st.text}
                          </span>
                        </button>
                      );
                    })}
                  </div>
                )}
              </div>
            </div>
          </div>

          {activeFocus && (
            <div className="dash-filterbar" role="status">
              <span className="chip on">
                <span className="dot" style={{ background: providerColor(activeFocus) }} />
                {activeFocus}
                <button
                  className="x"
                  type="button"
                  aria-label={`Clear the ${activeFocus} filter`}
                  onClick={() => setFocus(null)}
                >
                  <Icon name="close" size={9} strokeWidth={2.4} />
                </button>
              </span>
              <span className="dim">
                Types and regions below are scoped to {activeFocus} —{' '}
                <span className="mono">{fmtCount(scopedTotal)}</span> of{' '}
                <span className="mono">{fmtCount(assets.length)}</span> assets.
              </span>
            </div>
          )}

          <div className="dash-ranks">
            <div className="card">
              <div className="card-head">
                Top resource types
                {activeFocus && <span className="chip">{activeFocus}</span>}
                <span className="spacer" />
                <span className="hint">{fmtCount(agg.types.size)} distinct</span>
              </div>
              <div className="card-body">
                <BarList rows={typeRows} total={scopedTotal} />
              </div>
            </div>

            <div className="card">
              <div className="card-head">
                By region
                {activeFocus && <span className="chip">{activeFocus}</span>}
                <span className="spacer" />
                <span className="hint">{fmtCount(agg.regions.size)} distinct</span>
              </div>
              <div className="card-body">
                <BarList
                  rows={regionRows}
                  total={scopedTotal}
                  emptyText="No asset in this scope carries a region."
                />
              </div>
            </div>
          </div>
        </>
      )}

      {errorCount > 0 && (
        <div className="card">
          <div className="card-head">
            <button
              className="btn ghost sm"
              type="button"
              aria-expanded={errorsOpen}
              // Only while the panel exists: aria-controls pointing at an id
              // that is not in the document is a dangling reference.
              aria-controls={errorsOpen ? 'dash-errors' : undefined}
              onClick={() => setErrorsOpen(!errorsOpen)}
            >
              {/* The rotation lives on a wrapper because Icon exposes className,
                  not style; the reduced-motion override still governs it. */}
              <span
                className="dash-chev"
                style={{ transform: errorsOpen ? 'rotate(90deg)' : undefined }}
              >
                <Icon name="chevron" size={13} />
              </span>
              Errors
            </button>
            <span className="count-badge">{errorCount}</span>
            <span className="dim">results are partial</span>
            <span className="spacer" />
            <button className="btn ghost sm" type="button" onClick={copyErrors}>
              <Icon name="copy" size={13} />
              Copy all
            </button>
          </div>
          {errorsOpen && (
            <div className="card-body dash-errors" id="dash-errors">
              {errorGroups.map(([group, list]) => (
                <div className="dash-err-group" key={group}>
                  <div className="dash-err-head">
                    <span
                      className="dot"
                      style={{
                        background:
                          group === 'unattributed' ? 'var(--text-faint)' : providerColor(group),
                      }}
                    />
                    <strong>{group}</strong>
                    <span className="count-badge">{list.length}</span>
                  </div>
                  <ul className="dash-err-list">
                    {list.map((e, i) => (
                      <li className="mono" key={`${group}-${i}`}>
                        {e.init && <span className="pill warn">skipped</span>}
                        {e.msg}
                      </li>
                    ))}
                  </ul>
                </div>
              ))}
            </div>
          )}
        </div>
      )}
    </div>
  );
}
