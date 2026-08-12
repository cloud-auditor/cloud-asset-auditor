import type {
  Asset,
  AuditDone,
  DetailLevel,
  GroupBy,
  KubeContextsResponse,
  ProvidersResponse,
  ReachResult,
  Topology,
} from './types';

/**
 * The API lives on the same origin as the exported frontend — the Go binary
 * serves both — so every path here is relative. That is what lets the same
 * build work behind a port-forward, an Ingress sub-path, or `localhost:8080`
 * with no rebuild.
 */
const API = '/api/v1';

async function getJSON<T>(path: string, signal?: AbortSignal): Promise<T> {
  const res = await fetch(API + path, { signal, headers: { Accept: 'application/json' } });
  if (!res.ok) {
    throw new Error(`${path} → ${res.status} ${res.statusText}`);
  }
  return (await res.json()) as T;
}

export function fetchProviders(signal?: AbortSignal) {
  return getJSON<ProvidersResponse>('/providers', signal);
}

export function fetchKubeContexts(signal?: AbortSignal) {
  return getJSON<KubeContextsResponse>('/kube/contexts', signal);
}

/**
 * Which providers, and which clusters, a request that *runs an audit* should
 * ask. Every endpoint that collects — `/audit`, `/audit/export`, and the GET
 * form of `/topology` and `/reach` — reads the same two query params, so the
 * shape is declared once and extended by each endpoint's own knobs.
 *
 * The scope is deliberately the only provider-facing knob a browser client
 * gets: credentials, include-raw and region scope are operator configuration
 * set when `auditor serve` starts. `kube_contexts` is admissible because it
 * selects among clusters the operator already configured, and the server drops
 * any name absent from its kubeconfig (`validateKubeContexts`).
 */
export interface ScopeParams {
  providers?: string[];
  kubeContexts?: string[];
}

/** Serialises {@link ScopeParams} onto a query the collecting endpoints share. */
function appendScope(q: URLSearchParams, p: ScopeParams): void {
  if (p.providers?.length) q.set('providers', p.providers.join(','));
  if (p.kubeContexts?.length) q.set('kube_contexts', p.kubeContexts.join(','));
}

export interface AuditParams extends ScopeParams {
  timeout?: string;
}

function auditQuery(p: AuditParams): string {
  const q = new URLSearchParams();
  appendScope(q, p);
  if (p.timeout) q.set('timeout', p.timeout);
  const s = q.toString();
  return s ? `?${s}` : '';
}

/** Callbacks for {@link streamAudit}. Every one is optional. */
export interface AuditHandlers {
  onMeta?: (m: { started_at: string }) => void;
  onAsset?: (a: Asset) => void;
  onInitError?: (msg: string) => void;
  onError?: (msg: string) => void;
  onDone?: (d: AuditDone) => void;
  onFailure?: (err: Error) => void;
}

/**
 * Streams `GET /api/v1/audit` and dispatches each SSE event.
 *
 * Implemented over `fetch` + a manual parse rather than `EventSource` for two
 * reasons the API forces on us:
 *
 *  - EventSource cannot send an Authorization header, so it cannot reach a
 *    server started with `--auth token`. fetch inherits the browser's normal
 *    credential handling and works under basic auth.
 *  - EventSource auto-reconnects on stream end. This stream *always* ends —
 *    the audit finishes — so EventSource would restart the whole audit in a
 *    loop, hammering every provider API.
 *
 * Returns an abort function; calling it cancels the request and, because the
 * handler threads the request context into the providers, stops the audit
 * server-side too.
 */
export function streamAudit(params: AuditParams, h: AuditHandlers): () => void {
  const ctl = new AbortController();

  (async () => {
    try {
      const res = await fetch(`${API}/audit${auditQuery(params)}`, {
        signal: ctl.signal,
        headers: { Accept: 'text/event-stream' },
      });
      if (!res.ok || !res.body) {
        throw new Error(`audit → ${res.status} ${res.statusText}`);
      }

      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buf = '';

      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });

        // SSE frames are separated by a blank line. Anything after the last
        // separator is a partial frame and stays buffered for the next chunk.
        let sep: number;
        while ((sep = buf.indexOf('\n\n')) !== -1) {
          const frame = buf.slice(0, sep);
          buf = buf.slice(sep + 2);
          dispatch(frame, h);
        }
      }
    } catch (err) {
      // An abort is the caller's own doing, not a failure to report.
      if ((err as Error)?.name === 'AbortError') return;
      h.onFailure?.(err as Error);
    }
  })();

  return () => ctl.abort();
}

function dispatch(frame: string, h: AuditHandlers) {
  let event = 'message';
  const dataLines: string[] = [];
  for (const line of frame.split('\n')) {
    if (line.startsWith('event:')) event = line.slice(6).trim();
    else if (line.startsWith('data:')) dataLines.push(line.slice(5).trim());
  }
  if (!dataLines.length) return;

  let payload: unknown;
  try {
    payload = JSON.parse(dataLines.join('\n'));
  } catch {
    return; // a malformed frame must not kill the stream
  }

  switch (event) {
    case 'meta':
      h.onMeta?.(payload as { started_at: string });
      break;
    case 'asset':
      h.onAsset?.(payload as Asset);
      break;
    case 'init_error':
      h.onInitError?.((payload as { message: string }).message);
      break;
    case 'error':
      h.onError?.((payload as { message: string }).message);
      break;
    case 'done':
      h.onDone?.(payload as AuditDone);
      break;
  }
}

/**
 * The graph knobs, plus the audit scope inherited from {@link ScopeParams}.
 *
 * The scope only bites on the paths that actually collect — the GET in
 * {@link fetchTopology} and every {@link topologyDownloadURL}. {@link
 * buildTopology} POSTs assets the browser already holds, and `handleTopologyBuild`
 * reads no scope params at all, so they are inert there.
 */
export interface TopologyParams extends ScopeParams {
  detail?: DetailLevel;
  groupBy?: GroupBy;
  hostnames?: string[];
  includeOrphans?: boolean;
}

function topologyQuery(p: TopologyParams, format?: string): string {
  const q = new URLSearchParams();
  if (format) q.set('format', format);
  if (p.detail && p.detail !== 'low') q.set('detail', p.detail);
  if (p.groupBy) q.set('group_by', p.groupBy);
  if (p.includeOrphans) q.set('include-orphans', 'true');
  for (const h of p.hostnames ?? []) q.append('hostname', h);
  // Without this the scope picker was decorative on anything that collects:
  // "Run fresh audit" and all seven download links re-ran *every* registered
  // provider, which on a tenancy with OCI plus a large cluster is minutes of
  // work and API quota the operator had explicitly deselected.
  appendScope(q, p);
  const s = q.toString();
  return s ? `?${s}` : '';
}

/**
 * Builds a graph from assets the browser already holds, via
 * `POST /api/v1/topology`. This is the cheap path: the Assets view has the
 * full stream in memory, so re-running every provider just to draw a diagram
 * would be a waste of the operator's API quota.
 *
 * Caveat worth surfacing in the UI: the SSE stream only carries `raw` when
 * the server was started with `--include-raw`, and the Kubernetes resolvers
 * (Ingress/HTTPRoute → Service, Service → Pod, NetworkPolicy flows) parse
 * `raw`. Without it this path yields a thinner graph than {@link fetchTopology},
 * which forces include-raw on server-side.
 */
export async function buildTopology(
  assets: Asset[],
  params: TopologyParams,
  signal?: AbortSignal,
): Promise<Topology> {
  const res = await fetch(`${API}/topology${topologyQuery(params)}`, {
    method: 'POST',
    signal,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ assets }),
  });
  if (!res.ok) {
    throw new Error(`topology → ${res.status} ${await res.text()}`);
  }
  return (await res.json()) as Topology;
}

/** Runs a fresh raw-bearing audit server-side and returns the graph. Slow. */
export function fetchTopology(params: TopologyParams, signal?: AbortSignal) {
  return getJSON<Topology>(`/topology${topologyQuery(params)}`, signal);
}

/** Download URL for a rendered diagram (dot, mermaid, d2, excalidraw, …). */
export function topologyDownloadURL(format: string, params: TopologyParams): string {
  return `${API}/topology${topologyQuery(params, format)}`;
}

/** Download URL for the asset inventory in a renderer format. */
export function exportURL(format: string, params: AuditParams): string {
  const q = new URLSearchParams(auditQuery(params).replace(/^\?/, ''));
  q.set('format', format);
  return `${API}/audit/export?${q.toString()}`;
}

/**
 * The reachability question, plus the audit scope.
 *
 * Same split as {@link TopologyParams}: the scope reaches the collecting paths
 * ({@link fetchReach}, {@link reachDownloadURL}) and is inert on {@link
 * buildReach}, which posts assets the browser already holds.
 */
export interface ReachParams extends ScopeParams {
  from?: string;
  to?: string;
  exposed?: boolean;
  maxHops?: number;
  maxPaths?: number;
  kinds?: string[];
  includeDeny?: boolean;
}

function reachQuery(p: ReachParams, format?: string): string {
  const q = new URLSearchParams();
  if (format) q.set('format', format);
  if (p.from) q.set('from', p.from);
  if (p.to) q.set('to', p.to);
  if (p.exposed) q.set('exposed', 'true');
  if (p.maxHops) q.set('max_hops', String(p.maxHops));
  if (p.maxPaths) q.set('max_paths', String(p.maxPaths));
  if (p.kinds?.length) q.set('kinds', p.kinds.join(','));
  if (p.includeDeny) q.set('include_deny', 'true');
  // As in topologyQuery: the export links here run the query server-side
  // against a fresh audit, and without the scope that audit ignored the picker.
  appendScope(q, p);
  return q.toString() ? `?${q.toString()}` : '';
}

/**
 * Runs a reachability query against assets the browser already holds, via
 * `POST /api/v1/reach`.
 *
 * Same include-raw caveat as {@link buildTopology}: without raw payloads the
 * Kubernetes resolvers contribute no edges, so paths through a cluster are
 * missed. The UI says so when a result comes back empty.
 */
export async function buildReach(
  assets: Asset[],
  params: ReachParams,
  signal?: AbortSignal,
): Promise<ReachResult> {
  const res = await fetch(`${API}/reach${reachQuery(params)}`, {
    method: 'POST',
    signal,
    headers: { 'Content-Type': 'application/json', Accept: 'application/json' },
    body: JSON.stringify({ assets }),
  });
  if (!res.ok) {
    // The server explains selector misses and missing-selector errors in the
    // body; surfacing the status alone would hide the actionable part.
    throw new Error((await res.text()).trim() || `reach → ${res.status}`);
  }
  return (await res.json()) as ReachResult;
}

/** Runs a fresh raw-bearing audit server-side, then the query. Slow, complete. */
export function fetchReach(params: ReachParams, signal?: AbortSignal) {
  return getJSON<ReachResult>(`/reach${reachQuery(params)}`, signal);
}

/** Download URL for a rendered reachability diagram. */
export function reachDownloadURL(format: string, params: ReachParams): string {
  return `${API}/reach${reachQuery(params, format)}`;
}
