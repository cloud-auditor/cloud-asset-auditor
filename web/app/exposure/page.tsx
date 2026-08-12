'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { useAudit } from '@/components/AuditProvider';
import { AuditControls } from '@/components/AuditControls';
import { PathList } from '@/components/PathList';
import { StatTile } from '@/components/StatTile';
import { buildReach, reachDownloadURL, type ReachParams } from '@/lib/api';
import { toneColor, type Tone } from '@/lib/colors';
import { vars } from '@/lib/css';
import { Icon, type UIIconName } from '@/lib/icons';
import type { ReachResult } from '@/lib/types';
import './exposure.css';

type Mode = 'exposed' | 'to' | 'from' | 'trace';

const MODES: { id: Mode; label: string; icon: UIIconName; hint: string }[] = [
  {
    id: 'exposed',
    label: 'Internet exposure',
    icon: 'globe',
    hint: 'Starts from every public entry point the inventory found — a DNS record, a public load balancer — and walks inwards.',
  },
  {
    id: 'to',
    label: 'What can reach…',
    icon: 'target',
    hint: 'Everything upstream of an asset. Walks edges backwards from the selector.',
  },
  {
    id: 'from',
    label: 'What can … reach',
    icon: 'external',
    hint: 'Everything downstream of an asset. Walks edges forwards from the selector.',
  },
  {
    id: 'trace',
    label: 'Trace a route',
    icon: 'search',
    hint: 'Every simple route from one asset to another, shortest first.',
  },
];

/** The same seven graph formats the Topology page offers, plus json for the
 *  machine-readable answer. `RenderReach` collapses a result's paths back into
 *  a sub-topology and hands it to the ordinary renderers, so parity is a
 *  promise the server makes (internal/topology/reach_render.go) — listing
 *  fewer here would just hide two working exports. */
const DOWNLOAD_FORMATS = [
  'json',
  'dot',
  'mermaid',
  'd2',
  'graphml',
  'excalidraw',
  'drawio',
  'html',
] as const;

/** A <datalist> is a live DOM subtree the browser re-filters on every
 *  keystroke, and an audit routinely carries tens of thousands of assets — the
 *  full set would cost far more than the convenience is worth. This is a
 *  typeahead, not the set of legal inputs: the field still accepts any glob. */
const SUGGESTION_CAP = 300;

export default function ExposurePage() {
  const { assets, running, selectedProviders, selectedKubeContexts } = useAudit();

  const [mode, setMode] = useState<Mode>('exposed');
  const [from, setFrom] = useState('');
  const [to, setTo] = useState('');
  const [policyOnly, setPolicyOnly] = useState(false);
  const [includeDeny, setIncludeDeny] = useState(false);
  const [maxHops, setMaxHops] = useState(6);

  const [result, setResult] = useState<ReachResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The Assets drawer links here with a selector already chosen, so the page
  // has to accept one. Read in an effect, never during render: the export is
  // prerendered by `next build`, where there is no window.
  useEffect(() => {
    const q = new URLSearchParams(window.location.search);
    const f = q.get('from');
    const t = q.get('to');
    if (f && t) {
      setFrom(f);
      setTo(t);
      setMode('trace');
    } else if (f) {
      setFrom(f);
      setMode('from');
    } else if (t) {
      setTo(t);
      setMode('to');
    }
  }, []);

  // The audit scope rides along so the `export` links — which are GETs that
  // re-run the providers server-side — honour the picker above. Inert on the
  // POST `run()` takes, which asks about assets the browser already holds.
  const params = useMemo<ReachParams>(() => {
    const p: ReachParams = {
      maxHops,
      includeDeny,
      providers: selectedProviders.length > 0 ? selectedProviders : undefined,
      kubeContexts: selectedKubeContexts.length > 0 ? selectedKubeContexts : undefined,
    };
    if (policyOnly) p.kinds = ['traffic-allow', ...(includeDeny ? ['traffic-deny'] : [])];
    if (mode === 'exposed') p.exposed = true;
    if (mode === 'to' || mode === 'trace') p.to = to;
    if (mode === 'from' || mode === 'trace') p.from = from;
    return p;
  }, [mode, from, to, policyOnly, includeDeny, maxHops, selectedProviders, selectedKubeContexts]);

  const needsFrom = mode === 'from' || mode === 'trace';
  const needsTo = mode === 'to' || mode === 'trace';
  const ready =
    assets.length > 0 && (!needsFrom || from.trim() !== '') && (!needsTo || to.trim() !== '');

  const suggestions = useMemo(() => {
    const out: string[] = [];
    const seen = new Set<string>();
    for (const a of assets) {
      for (const v of [a.name, a.id]) {
        if (!v || seen.has(v)) continue;
        seen.add(v);
        out.push(v);
        if (out.length >= SUGGESTION_CAP) return out;
      }
    }
    return out;
  }, [assets]);

  const run = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setResult(await buildReach(assets, params));
    } catch (e) {
      setResult(null);
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [assets, params]);

  // `?trace=1` runs the query once a run has finished, so a link can deliver a
  // finished answer with no interaction — the counterpart to `?run=1` on the
  // audit and `?build=1` on the topology page. Combined with `?from=`/`?to=`
  // it makes "here is what can reach the database" a single shareable URL.
  //
  // It waits for the run to END: reachability computed over a partial
  // inventory understates exposure, and understating exposure is the one
  // direction this tool must never be wrong in.
  const autoTraced = useRef(false);
  useEffect(() => {
    if (autoTraced.current || running || assets.length === 0) return;
    const q = new URLSearchParams(window.location.search);
    if (q.get('trace') !== '1' && q.get('trace') !== 'true') return;
    autoTraced.current = true;
    void run();
  }, [running, assets.length, run]);

  const active = MODES.find((m) => m.id === mode);

  const stats = useMemo(() => {
    if (!result) return null;
    const ends = new Set<string>();
    let deny = 0;
    let depth = 0;
    for (const p of result.paths) {
      const end = p.nodes[p.nodes.length - 1];
      if (end) ends.add(`${end.provider} ${end.id}`);
      if (p.edges.some((e) => e.kind === 'traffic-deny')) deny += 1;
      depth = Math.max(depth, p.edges.length);
    }
    return { ends: ends.size, deny, depth };
  }, [result]);

  const resultErrors = (result?.init_errors ?? []).concat(result?.errors ?? []);

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Exposure</h1>
          <p>Trace what can reach what — across DNS, load balancers, clusters, and policy.</p>
        </div>
        <div className="spacer" />
        {result && (
          <div
            className="exports"
            title="These re-run the providers server-side with raw payloads, so they answer the same question against a fuller graph — and take as long as an audit."
          >
            <span className="label">
              <Icon name="download" size={12} />
              export
            </span>
            {DOWNLOAD_FORMATS.map((f) => (
              <a key={f} className="btn sm" href={reachDownloadURL(f, params)}>
                {f}
              </a>
            ))}
          </div>
        )}
      </div>

      <AuditControls />

      <div className="card" style={{ marginBottom: 16 }}>
        <form
          className="card-body exp-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (ready && !loading && !running) void run();
          }}
        >
          <div className="exp-modes">
            <ModePicker
              mode={mode}
              onChange={(m) => {
                setMode(m);
                setResult(null);
              }}
            />
            {active && <span className="exp-mode-hint">{active.hint}</span>}
          </div>

          <div className="exp-fields">
            {needsFrom && (
              <label className="field exp-field">
                From
                <input
                  value={from}
                  onChange={(e) => setFrom(e.target.value)}
                  placeholder="api.example.com"
                  list="exp-selectors"
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
            )}
            {needsTo && (
              <label className="field exp-field">
                To
                <input
                  value={to}
                  onChange={(e) => setTo(e.target.value)}
                  placeholder="*postgres*"
                  list="exp-selectors"
                  autoComplete="off"
                  spellCheck={false}
                />
              </label>
            )}
            <label className="field">
              Max hops
              <input
                type="number"
                min={1}
                max={20}
                value={maxHops}
                onChange={(e) => setMaxHops(Number(e.target.value) || 6)}
                style={{ width: 84 }}
              />
            </label>
            <button className="primary" disabled={!ready || loading || running} type="submit">
              {loading ? 'Tracing…' : 'Trace'}
            </button>
          </div>

          {(needsFrom || needsTo) && (
            <>
              <p className="hint exp-glob">
                Selectors are case-insensitive globs matched against <strong>both</strong> the asset
                id and its name, so <code>api.example.com</code> and{' '}
                <code>ocid1.loadbalancer.*</code> both work without saying which you meant.{' '}
                <code>*</code> matches anything, and a selector matching several assets seeds the
                walk from all of them.
                {suggestions.length > 0 &&
                  (suggestions.length >= SUGGESTION_CAP
                    ? ' The suggestions are a sample of the names and ids this audit collected — you can still type any glob.'
                    : ' The suggestions are the names and ids this audit collected — you can still type any glob.')}
              </p>

              {/* One list serves both fields; they draw from the same inventory. */}
              <datalist id="exp-selectors">
                {suggestions.map((s) => (
                  <option key={s} value={s} />
                ))}
              </datalist>
            </>
          )}

          <div className="row">
            <button
              type="button"
              className={`chip${policyOnly ? ' on' : ''}`}
              aria-pressed={policyOnly}
              onClick={() => setPolicyOnly(!policyOnly)}
              title="Follow only NetworkPolicy / Tailscale ACL / NetBird policy edges — the 'who may talk to whom' view, with the plumbing left out"
            >
              Policy edges only
            </button>
            <button
              type="button"
              className={`chip${includeDeny ? ' on' : ''}`}
              aria-pressed={includeDeny}
              onClick={() => setIncludeDeny(!includeDeny)}
              title="A deny edge states traffic does NOT flow, so it is skipped by default — traversing one would manufacture routes policy forbids. Turn it on to audit the denials themselves."
            >
              Follow deny rules
            </button>
          </div>
        </form>
      </div>

      {/* Alert glyph on anything reporting a problem, nothing on an
          informational note — the same rule the other pages follow. */}
      {error && (
        <div className="banner error">
          <Icon name="alert" size={15} />
          <span>{error}</span>
        </div>
      )}

      {!result || !stats ? (
        <div className="card">
          <div className="empty">
            <Icon name="target" size={30} strokeWidth={1.3} />
            <h3>{assets.length === 0 ? 'Nothing to trace yet' : (active?.label ?? 'Ready')}</h3>
            <p>
              {assets.length === 0
                ? 'Reachability is computed from a collected inventory. Run an audit above, then ask a question here.'
                : needsFrom || needsTo
                  ? 'Enter a selector and press Trace. The graph is built from the assets already in the browser — no provider is re-queried.'
                  : 'Press Trace to walk inwards from every public entry point the inventory found.'}
            </p>
          </div>
        </div>
      ) : (
        <>
          {/* Numbers, not pre-formatted strings: StatTile counts a numeric
              value up to its new one, which is the whole reason it exists. */}
          <div className="stats stagger" style={{ marginBottom: 16 }}>
            <StatTile
              icon="layers"
              label="Paths"
              value={result.paths.length}
              sub={result.truncated ? 'capped — more exist' : undefined}
              color={result.truncated ? tone('warn') : undefined}
            />
            <StatTile
              icon="target"
              label={mode === 'to' ? 'Distinct sources' : 'Assets reached'}
              value={stats.ends}
            />
            {result.sources && result.sources.length > 0 && (
              <StatTile
                icon="globe"
                label={mode === 'exposed' ? 'Entry points' : 'Matched sources'}
                value={result.sources.length}
              />
            )}
            <StatTile
              icon="alert"
              label="Crossing a deny"
              value={stats.deny}
              color={stats.deny > 0 ? tone('err') : undefined}
              sub={stats.deny > 0 ? 'a rule says no on this route' : undefined}
            />
            <StatTile icon="rows" label="Max depth" value={stats.depth} sub="hops" />
          </div>

          {result.truncated && (
            <div className="banner warn">
              <Icon name="alert" size={15} />
              <span>
                Result truncated — more routes exist than were returned. Treat this as{' '}
                <em>at least</em> this much reachability, not all of it.
              </span>
            </div>
          )}

          {resultErrors.length > 0 && (
            <div className="banner warn">
              <Icon name="alert" size={15} />
              <span>
                {resultErrors.length} provider error{resultErrors.length === 1 ? '' : 's'} while
                building the graph — the answer below is drawn from an incomplete inventory.
                <span className="mono faint" style={{ display: 'block', marginTop: 4 }}>
                  {resultErrors[0]}
                </span>
              </span>
            </div>
          )}

          {result.paths.length === 0 ? (
            <ZeroResult mode={mode} maxHops={maxHops} policyOnly={policyOnly} includeDeny={includeDeny} />
          ) : (
            <div className="card flush">
              <div className="card-head">
                {result.question}
                <span className="spacer" />
                <span className="hint">shortest route first</span>
              </div>
              <PathList
                paths={result.paths}
                endLabel={mode === 'to' ? 'Source' : 'Destination'}
              />
            </div>
          )}
        </>
      )}
    </>
  );
}

/** The mode picker. A segmented control rather than four chips because the
 *  modes are mutually exclusive — chips read as filters that combine. */
function ModePicker({ mode, onChange }: { mode: Mode; onChange: (m: Mode) => void }) {
  const segRef = useRef<HTMLDivElement | null>(null);
  const btnRefs = useRef(new Map<Mode, HTMLButtonElement>());
  const [thumb, setThumb] = useState<{ x: number; w: number } | null>(null);

  // Same measurement as the nav's: offsetLeft is relative to the offsetParent's
  // padding edge, which is the origin `left: 0` resolves against, so no rect
  // subtraction is needed. Until it lands `thumb` is null and globals.css falls
  // back to the active button painting its own pill.
  useEffect(() => {
    const seg = segRef.current;
    const el = btnRefs.current.get(mode);
    if (!seg || !el) {
      setThumb(null);
      return;
    }
    const measure = () => setThumb({ x: el.offsetLeft, w: el.offsetWidth });
    measure();
    const ro = new ResizeObserver(measure);
    ro.observe(seg);
    ro.observe(el);
    return () => ro.disconnect();
  }, [mode]);

  return (
    <div className="segmented accent" role="group" aria-label="Reachability question" ref={segRef}>
      {thumb && (
        <span className="thumb" aria-hidden="true" style={vars({ '--seg-x': `${thumb.x}px`, '--seg-w': `${thumb.w}px` })} />
      )}
      {MODES.map((m) => {
        const on = m.id === mode;
        return (
          <button
            key={m.id}
            type="button"
            data-on={on ? 'true' : 'false'}
            aria-pressed={on}
            onClick={() => onChange(m.id)}
            ref={(el) => {
              if (el) btnRefs.current.set(m.id, el);
              else btnRefs.current.delete(m.id);
            }}
          >
            <Icon name={m.icon} size={13} />
            {m.label}
          </button>
        );
      })}
    </div>
  );
}

/** StatTile takes a colour, not a tone — it has no opinion about semantics.
 *  This keeps the call sites naming the meaning rather than the swatch. */
function tone(t: Tone): string {
  return toneColor(t);
}

/**
 * The zero-result state.
 *
 * This is the most consequential copy on the page. The graph is *inferred* from
 * whatever the audit managed to collect, so a missing path is at least as often
 * a gap in the inventory as a gap in the network — and an operator who reads
 * "0 paths" as "safe" has been actively misled by the tool. The reasons are
 * listed specifically, not hedged in general terms, because a vague caveat gets
 * skipped and a specific one tells the reader what to go and check.
 */
function ZeroResult({
  mode,
  maxHops,
  policyOnly,
  includeDeny,
}: {
  mode: Mode;
  maxHops: number;
  policyOnly: boolean;
  includeDeny: boolean;
}) {
  return (
    <div className="card surface-rail exp-caveat" style={vars({ '--rail': 'var(--warn)' })}>
      <div className="card-body">
        <div className="exp-caveat-head">
          <Icon name="alert" size={20} />
          <h2>No path found — which is not proof of isolation</h2>
        </div>

        <p>
          Read this as <em>&ldquo;nothing in the collected data connects them&rdquo;</em>, never as
          &ldquo;nothing connects them&rdquo;. Every edge in this graph was inferred from an asset
          the audit happened to collect, so silence here is weak evidence — far weaker than the
          positive finding you would get if a route did exist.
        </p>

        <ul className="exp-reasons">
          <li>
            <strong>No raw payloads in the browser.</strong> This page builds the graph from the
            assets already streamed to it, and the SSE stream only carries <code>raw</code> when the
            server was started with <code>--include-raw</code>. The Kubernetes resolvers —
            Ingress/HTTPRoute → Service, Service selector → Pod, NetworkPolicy flows — parse{' '}
            <code>raw</code>, so without it every hop inside a cluster is simply absent. The{' '}
            <code>export</code> links above run the same query server-side, where raw is forced on.
          </li>
          <li>
            <strong>A resolver with nothing to join.</strong> &ldquo;Who may talk to whom&rdquo; is
            read out of policy documents — NetworkPolicies, Tailscale ACLs, NetBird policies. A
            cluster with no NetworkPolicy at all produces no traffic edges, which looks identical to
            a cluster that forbids everything.
          </li>
          <li>
            <strong>A cross-provider join that needs an address match.</strong> A DNS record only
            links to a load balancer if the record&apos;s value matches an IP or hostname the
            inventory actually collected. A CNAME to a third party, an address on a resource the
            credentials could not read, or an LB whose IP the collector omitted all leave a gap
            where an edge belongs.
          </li>
          <li>
            <strong>The traversal is bounded on purpose.</strong> This query stopped at{' '}
            {maxHops} hop{maxHops === 1 ? '' : 's'}
            {!includeDeny && ', skipped deny rules'}
            {policyOnly && ', and followed policy edges only'}. Widening any of those can surface
            routes this run did not enumerate.
          </li>
          <li>
            <strong>The inventory is only what the credentials could see.</strong> A provider that
            was skipped, a compartment outside the scope, or a token missing a read permission
            removes assets — and every edge that would have touched them.
          </li>
        </ul>

        <p className="exp-caveat-foot">
          {mode === 'exposed'
            ? 'A clean exposure result is a good sign and not a clearance.'
            : 'A route found here is evidence. A route not found is the absence of evidence.'}
        </p>
      </div>
    </div>
  );
}
