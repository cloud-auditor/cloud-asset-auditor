'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import './topology.css';
import { useAudit } from '@/components/AuditProvider';
import { AuditControls } from '@/components/AuditControls';
import { GraphCanvas } from '@/components/GraphCanvas';
import { buildTopology, fetchTopology, topologyDownloadURL, type TopologyParams } from '@/lib/api';
import { Icon } from '@/lib/icons';
import { EDGE_KINDS, type DetailLevel, type GroupBy, type Topology } from '@/lib/types';

/**
 * Above this many nodes the force layout stops being useful — the picture is
 * a hairball and the O(n²) repulsion pass starts dropping frames. Rather than
 * render it anyway, the page refuses and points at the detail levels, which
 * exist precisely for this.
 */
const MAX_RENDERED_NODES = 900;

const DOWNLOAD_FORMATS = ['dot', 'mermaid', 'd2', 'graphml', 'excalidraw', 'drawio', 'html'] as const;

const DETAILS: { id: DetailLevel; label: string; hint: string }[] = [
  { id: 'low', label: 'Every asset', hint: 'One node per collected asset' },
  { id: 'medium', label: 'By type', hint: 'One node per group and resource type' },
  { id: 'high', label: 'Overview', hint: 'One node per group — the network diagram' },
];

const GROUPS: { id: GroupBy; label: string }[] = [
  { id: 'provider', label: 'Provider' },
  { id: 'account', label: 'Account' },
  { id: 'region', label: 'Region' },
  { id: '', label: 'Flat' },
];

export default function TopologyPage() {
  const { assets, running, selectedProviders, selectedKubeContexts } = useAudit();

  const [detail, setDetail] = useState<DetailLevel>('low');
  const [groupBy, setGroupBy] = useState<GroupBy>('provider');
  const [includeOrphans, setIncludeOrphans] = useState(false);
  const [kinds, setKinds] = useState<Set<string>>(new Set());
  const [focusId, setFocusId] = useState<string | null>(null);

  const [topo, setTopo] = useState<Topology | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // The scope reaches `topologyQuery` in lib/api.ts, which serialises it onto
  // the paths that actually collect: "Run fresh audit" and every download link.
  // It is inert on `buildFromStream`, whose POST body already carries the
  // assets and whose handler reads no scope params.
  const params = useMemo<TopologyParams>(
    () => ({
      detail,
      groupBy,
      includeOrphans,
      providers: selectedProviders.length > 0 ? selectedProviders : undefined,
      kubeContexts: selectedKubeContexts.length > 0 ? selectedKubeContexts : undefined,
    }),
    [detail, groupBy, includeOrphans, selectedProviders, selectedKubeContexts],
  );

  /** Builds from the assets already in the browser — no provider calls. */
  const buildFromStream = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setTopo(await buildTopology(assets, params));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [assets, params]);

  /** Runs a fresh server-side audit with raw payloads — slower, richer. */
  const buildFresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setTopo(await fetchTopology(params));
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setLoading(false);
    }
  }, [params]);

  // Re-derive the graph whenever a *shape* knob changes, as long as there is
  // already a graph on screen. Rebuilding is a cheap local POST; making the
  // user press a button after every toggle would be busywork. Deliberately
  // not keyed on `params`: that also moves when the scope picker moves, and a
  // scope change has no effect until the next run.
  useEffect(() => {
    if (topo && assets.length > 0) void buildFromStream();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [detail, groupBy, includeOrphans]);

  const booted = useRef(false);
  useEffect(() => {
    if (booted.current) return;
    booted.current = true;
    // window.location, not useSearchParams: this page is prerendered by
    // `next build`, where useSearchParams forces the whole tree into a
    // Suspense boundary or fails the export outright.
    const id = new URLSearchParams(window.location.search).get('focus');
    if (!id) return;
    setFocusId(id);
    // Arriving from "Show in topology" means the graph is the entire point of
    // the navigation — make it appear without demanding a second click.
    if (assets.length > 0) void buildFromStream();
    // Mount only: a later change to `assets` must not replay the deep link.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  // `?build=1` renders the graph as soon as a run finishes, so one URL can
  // deliver a finished diagram with no interaction — what a kiosk display, a
  // screenshot run, or a link pasted into an incident channel all need. It is
  // the counterpart to `?run=1` on the audit itself (see AuditProvider).
  //
  // It waits for the run to END rather than firing on the first assets: a
  // graph built from a third of the inventory is not an early version of the
  // real one, it is a different and quietly wrong one.
  const autoBuilt = useRef(false);
  useEffect(() => {
    if (autoBuilt.current || running || assets.length === 0) return;
    const q = new URLSearchParams(window.location.search);
    if (q.get('build') !== '1' && q.get('build') !== 'true') return;
    autoBuilt.current = true;
    void buildFromStream();
  }, [running, assets.length, buildFromStream]);

  const trafficEdges = useMemo(
    () =>
      (topo?.edges ?? []).filter(
        (e) => e.kind === EDGE_KINDS.trafficAllow || e.kind === EDGE_KINDS.trafficDeny,
      ).length,
    [topo],
  );

  const availableKinds = useMemo(() => {
    const s = new Set<string>();
    for (const e of topo?.edges ?? []) s.add(e.kind);
    return s;
  }, [topo]);

  /**
   * An empty set means "draw every kind" — the same encoding the provider
   * picker uses, and the same trap: without materialising the full set first,
   * the first click on a lit chip would *isolate* that kind rather than switch
   * it off, which is the opposite of what a pressed toggle promises.
   *
   * The two ends are handled like AuditControls handles its providers: a click
   * that would leave nothing selected is refused (there is no way to spell
   * "none" in this encoding, and a graph with no edges is not a useful state),
   * and a set that has grown to cover everything collapses back to empty so
   * there is exactly one representation of "all".
   */
  const toggleKind = useCallback(
    (k: string) => {
      setKinds((prev) => {
        const next = prev.size === 0 ? new Set(availableKinds) : new Set(prev);
        if (next.has(k)) next.delete(k);
        else next.add(k);
        if (next.size === 0) return prev;
        return next.size === availableKinds.size ? new Set<string>() : next;
      });
    },
    [availableKinds],
  );

  const resetKinds = useCallback(() => setKinds(new Set()), []);

  // A rebuilt graph can drop an edge kind entirely (switching to `high` detail
  // collapses most of them away). A filter naming kinds that no longer exist
  // would silently draw nothing, so it is pruned back to "all".
  useEffect(() => {
    setKinds((prev) => {
      if (prev.size === 0) return prev;
      const keep = new Set([...prev].filter((k) => availableKinds.has(k)));
      if (keep.size === prev.size) return prev;
      return keep.size === 0 || keep.size === availableKinds.size ? new Set<string>() : keep;
    });
  }, [availableKinds]);

  const tooBig = (topo?.nodes.length ?? 0) > MAX_RENDERED_NODES;
  const canBuild = !loading && !running;

  return (
    <>
      <div className="page-head">
        <div>
          <h1>Topology</h1>
          <p>Request paths and traffic-flow policy across every provider.</p>
        </div>
        <div className="spacer" />
        <div className="exports">
          <span className="label">
            <Icon name="download" size={12} />
            export
          </span>
          {DOWNLOAD_FORMATS.map((f) => (
            <a key={f} className="btn sm" href={topologyDownloadURL(f, params)}>
              {f}
            </a>
          ))}
        </div>
      </div>

      <AuditControls />

      <div className="card" style={{ marginBottom: 16 }}>
        <div className="topo-knobs">
          <div className="topo-knob">
            <span className="label">Detail</span>
            {/* No sliding .thumb on these two: the indicator has to be measured
                from the active button, and globals.css already falls back to
                the active button painting its own pill. Two more ResizeObservers
                on a page that runs a physics loop is not a good trade. */}
            <div className="segmented" role="group" aria-label="Detail level">
              {DETAILS.map((d) => (
                <button
                  key={d.id}
                  type="button"
                  data-on={detail === d.id ? 'true' : 'false'}
                  aria-pressed={detail === d.id}
                  title={d.hint}
                  onClick={() => setDetail(d.id)}
                >
                  {d.label}
                </button>
              ))}
            </div>
          </div>

          <div className="topo-knob">
            <span className="label">Group by</span>
            <div className="segmented" role="group" aria-label="Grouping dimension">
              {GROUPS.map((g) => (
                <button
                  key={g.id || 'flat'}
                  type="button"
                  data-on={groupBy === g.id ? 'true' : 'false'}
                  aria-pressed={groupBy === g.id}
                  onClick={() => setGroupBy(g.id)}
                >
                  {g.label}
                </button>
              ))}
            </div>
          </div>

          <button
            type="button"
            className={`chip${includeOrphans ? ' on' : ''}`}
            aria-pressed={includeOrphans}
            title="Assets no resolver could join to anything else"
            onClick={() => setIncludeOrphans((v) => !v)}
          >
            Show unconnected
          </button>

          <div className="spacer" />

          <button
            className="primary"
            onClick={buildFromStream}
            disabled={!canBuild || assets.length === 0}
            type="button"
            title="Build the graph from the assets this browser already holds — no provider calls"
          >
            {loading ? 'Building…' : 'Build from streamed assets'}
          </button>
          <button
            onClick={buildFresh}
            disabled={!canBuild}
            type="button"
            title="Run a fresh server-side audit with raw payloads — slower, but the Kubernetes resolvers need them"
          >
            Run fresh audit
          </button>
        </div>
      </div>

      {/* A banner that reports something going wrong carries the alert glyph;
          an informational note does not — the mark is what separates the two
          at a glance, so it has to mean one thing. Same rule on every page. */}
      {error && (
        <div className="banner error">
          <Icon name="alert" size={15} />
          <span>{error}</span>
        </div>
      )}
      {topo?.init_errors?.map((e, i) => (
        <div className="banner warn" key={i}>
          <Icon name="alert" size={15} />
          <span>Provider skipped: {e}</span>
        </div>
      ))}
      {topo && trafficEdges === 0 && topo.nodes.length > 0 && (
        <div className="banner info">
          No traffic-flow edges. They come from Kubernetes NetworkPolicies, Tailscale ACL rules, and
          NetBird policies — and the Kubernetes ones need raw payloads, which the streamed assets only
          carry when the server runs with <code>--include-raw</code>. Use{' '}
          <strong>Run fresh audit</strong>, which forces it on.
        </div>
      )}

      {!topo ? (
        <div className="card topo-guard">
          <div className="empty">
            <Icon name="shapes" size={30} strokeWidth={1.2} />
            {loading ? (
              <>
                <h3>Building the graph…</h3>
                <p>Running the resolvers over {assets.length.toLocaleString()} assets.</p>
              </>
            ) : assets.length > 0 ? (
              <>
                <h3>{assets.length.toLocaleString()} assets ready</h3>
                <p>
                  Build the graph from what this browser already holds — instant, and it costs no
                  provider API calls.
                </p>
                <div className="row">
                  <button className="primary" onClick={buildFromStream} type="button">
                    Build from streamed assets
                  </button>
                </div>
              </>
            ) : (
              <>
                <h3>Nothing to draw yet</h3>
                <p>
                  Run an audit, then build the graph. A fresh server-side run also collects the raw
                  payloads the Kubernetes resolvers read, so it finds traffic-flow edges the streamed
                  assets cannot.
                </p>
                <div className="row">
                  <button className="primary" onClick={buildFresh} disabled={!canBuild} type="button">
                    <Icon name="play" size={13} />
                    Run fresh audit
                  </button>
                </div>
              </>
            )}
          </div>
        </div>
      ) : tooBig ? (
        <div className="card topo-guard">
          <div className="empty">
            <Icon name="layers" size={30} strokeWidth={1.2} />
            <span className="n">{topo.nodes.length.toLocaleString()}</span>
            <h3>Too many nodes to lay out legibly</h3>
            <p>
              Past about {MAX_RENDERED_NODES.toLocaleString()} the force layout draws a hairball and
              the all-pairs repulsion starts dropping frames. Collapse the graph instead, or take it
              somewhere built for the scale — GraphML opens in yEd, Gephi, or Cytoscape, which lay out
              tens of thousands of nodes.
            </p>
            <div className="row">
              <button className="primary" onClick={() => setDetail('medium')} type="button">
                Collapse to one node per type
              </button>
              <a className="btn" href={topologyDownloadURL('graphml', params)}>
                <Icon name="download" size={13} />
                GraphML
              </a>
            </div>
          </div>
        </div>
      ) : (
        <>
          <div className="topo-counts">
            <span>
              <b>{topo.nodes.length.toLocaleString()}</b> nodes
            </span>
            <span>
              <b>{topo.edges.length.toLocaleString()}</b> edges
            </span>
            {trafficEdges > 0 && (
              <span>
                <b>{trafficEdges.toLocaleString()}</b> traffic-flow
              </span>
            )}
            <span className="spacer" />
            <span className="faint">drag to pan · scroll to zoom · drag a node to move it</span>
          </div>
          <GraphCanvas
            nodes={topo.nodes}
            edges={topo.edges}
            kinds={kinds}
            onToggleKind={toggleKind}
            onResetKinds={resetKinds}
            groupBy={groupBy}
            detail={detail}
            focusId={focusId}
          />
        </>
      )}
    </>
  );
}
