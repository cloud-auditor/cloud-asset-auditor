/**
 * Mirrors of the Go types the API serves. Kept hand-written rather than
 * generated from openapi.yaml so the frontend has no build-time dependency on
 * the spec file — but they must be kept in step with `internal/core`.
 */

/** Mirrors core.Asset (internal/core/asset.go). */
export interface Asset {
  provider: string;
  account_id: string;
  region?: string;
  type: string;
  id: string;
  name: string;
  status?: string;
  created_at?: string;
  tags?: Record<string, string>;
  raw?: unknown;
}

/** Mirrors core.AssetRef. */
export interface AssetRef {
  provider: string;
  account_id?: string;
  type: string;
  id: string;
}

/** Mirrors core.Edge. `count` is set only on collapsed (summarised) edges. */
export interface Edge {
  from: AssetRef;
  to: AssetRef;
  kind: string;
  hostname?: string;
  port?: number;
  confidence: 'exact' | 'heuristic';
  count?: number;
}

export interface Topology {
  nodes: Asset[];
  edges: Edge[];
  init_errors?: string[];
  errors?: string[];
}

export interface ProvidersResponse {
  providers: string[];
  auth_mode: string;
}

export interface KubeContextsResponse {
  contexts: string[];
  current: string;
  error?: string;
}

/** Terminal payload of the audit SSE stream (`event: done`). */
export interface AuditDone {
  count: number;
  elapsed_ms: number;
  errors: number;
}

/**
 * One second of the audit's arrival history, for the throughput sparkline.
 *
 * Buckets advance on the wall clock, not on traffic: a provider that stalls
 * contributes `n: 0` seconds, because a gap drawn as a continuous line would
 * read as steady throughput — the opposite of what happened.
 */
export interface ArrivalBucket {
  /** Bucket start, epoch ms, floored to the second. */
  t: number;
  /** Assets that landed in that second. */
  n: number;
}

/**
 * Per-provider progress, accumulated while the stream runs.
 *
 * Timestamps are epoch ms, taken at the batch flush that carried the asset
 * rather than at the SSE event — up to 200 ms late, which is irrelevant next to
 * the collection times these measure. `errors` counts both collect-time errors
 * and the init failure that skipped the provider entirely, so a provider with
 * `count: 0, errors: 1` reads as "tried, failed" rather than "not asked".
 */
export interface ProviderStat {
  count: number;
  firstAt: number;
  lastAt: number;
  errors: number;
}

export type ToastKind = 'ok' | 'warn' | 'err' | 'info';

/** A queued notification. `id` comes from a monotonic counter in AuditProvider. */
export interface Toast {
  id: number;
  kind: ToastKind;
  title: string;
  body?: string;
}

/** The canonical edge kinds, mirroring the constants in core/edge.go. */
export const EDGE_KINDS = {
  dns: 'dns',
  waf: 'waf',
  lbBackend: 'lb-backend',
  gatewayRoute: 'gateway-route',
  serviceBackend: 'service-backend',
  networkContainment: 'network-containment',
  trafficAllow: 'traffic-allow',
  trafficDeny: 'traffic-deny',
} as const;

/** Type given to the synthetic nodes produced by a `detail=medium|high` collapse. */
export const COLLAPSED_NODE_TYPE = 'topology.group';

export type DetailLevel = 'low' | 'medium' | 'high';
export type GroupBy = '' | 'provider' | 'account' | 'region';

/** Mirrors topology.Path — one route through the graph. */
export interface Path {
  nodes: Asset[];
  edges: Edge[];
}

/** Mirrors topology.ReachResult plus the server's error fields. */
export interface ReachResult {
  question: string;
  sources?: Asset[];
  targets?: Asset[];
  paths: Path[];
  /** True when max_paths was hit and more routes exist. */
  truncated: boolean;
  init_errors?: string[];
  errors?: string[];
}

/* `ReachParams` used to live here. It moved to lib/api.ts, alongside
   AuditParams and TopologyParams: it is a client-side query-string shape, not a
   mirror of a Go type, and it needs to extend the shared audit-scope fields
   those two already carry. */
