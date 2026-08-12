'use client';

/**
 * Asset-type glyphs.
 *
 * These are the same categories and the same drawings as the Excalidraw
 * exporter's icon set (internal/topology/excalidraw_icons.go) — deliberately,
 * so a node in the browser diagram and the same node in an exported
 * .excalidraw file read as the same thing. `iconKeyForType` below mirrors
 * `iconKeyForType` there, including the ordering: more specific substrings are
 * tested before broad ones, so "load_balancer" cannot be swallowed by the
 * generic network bucket.
 *
 * Drawn in-repo rather than vendored from an icon package: the bundle is
 * embedded in the Go binary, and a brand icon pack brings trademark baggage
 * along with the bytes (see CLAUDE.md, mistake 4).
 *
 * Glyphs stroke with `currentColor` so a caller tints them by provider, by
 * status, or not at all. `iconTint` exposes the exporter's per-category colour
 * for the places that want the icon to carry its own family identity.
 */

export type IconKey =
  | 'dns'
  | 'waf'
  | 'tunnel'
  | 'loadbalancer'
  | 'gateway'
  | 'service'
  | 'compute'
  | 'database'
  | 'storage'
  | 'network'
  | 'certificate'
  | 'account'
  | 'workload'
  | 'function'
  | 'generic';

/** Maps an asset Type to a glyph. Mirrors the Go implementation exactly. */
export function iconKeyForType(type: string): IconKey {
  const t = type.toLowerCase();
  const has = (s: string) => t.includes(s);
  const ends = (s: string) => t.endsWith(s);

  if (has('dns') || has('zone')) return 'dns';
  if (has('certificate') || has('mtls')) return 'certificate';
  if (has('ruleset') || has('access') || has('waf') || has('page_rule') || has('networkpolicy')) {
    return 'waf';
  }
  if (has('tunnel')) return 'tunnel';
  if (has('load_balancer') || has('loadbalancer') || (ends('.service') && has('lb'))) {
    return 'loadbalancer';
  }
  if (has('ingress') || has('httproute') || has('gatewayclass') || ends('.gateway') || has('route')) {
    return 'gateway';
  }
  if (ends('.service') || ends('.endpoints') || has('oke') || has('cluster')) return 'service';
  if (has('function') || has('worker_script') || has('pages_project')) return 'function';
  if (has('database') || has('db_system') || has('kv_namespace')) return 'database';
  if (
    has('bucket') ||
    has('object_storage') ||
    has('volume') ||
    has('r2_') ||
    has('persistentvolume') ||
    has('configmap') ||
    has('secret')
  ) {
    return 'storage';
  }
  if (has('vcn') || has('subnet') || has('_gateway') || has('drg') || has('peering') || has('vault')) {
    return 'network';
  }
  if (has('compute') || has('instance') || ends('.node') || has('container')) return 'compute';
  if (
    ends('.pod') ||
    has('deployment') ||
    has('replicaset') ||
    has('statefulset') ||
    has('daemonset') ||
    has('application') ||
    has('job')
  ) {
    return 'workload';
  }
  if (
    has('account') ||
    has('compartment') ||
    has('iam') ||
    has('user') ||
    has('group') ||
    has('policy') ||
    ends('.namespace') ||
    has('serviceaccount')
  ) {
    return 'account';
  }
  return 'generic';
}

/** The glyph bodies, as raw SVG children of a 24×24 viewBox. */
const BODIES: Record<IconKey, React.ReactNode> = {
  dns: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12h18M12 3c2.6 2.7 2.6 15.3 0 18M12 3c-2.6 2.7-2.6 15.3 0 18" />
    </>
  ),
  waf: (
    <>
      <path d="M12 3l7 3v5c0 4.6-3 7.6-7 9-4-1.4-7-4.4-7-9V6z" />
      <path d="M9 12l2 2 4-4" />
    </>
  ),
  tunnel: (
    <>
      <path d="M3 20V12a9 9 0 0 1 18 0v8" />
      <path d="M8 20v-6a4 4 0 0 1 8 0v6" />
    </>
  ),
  loadbalancer: (
    <>
      <circle cx="5" cy="12" r="2" />
      <circle cx="19" cy="5" r="2" />
      <circle cx="19" cy="12" r="2" />
      <circle cx="19" cy="19" r="2" />
      <path d="M7 12h2M11 12l6-6M11 12h6M11 12l6 6" />
    </>
  ),
  gateway: (
    <>
      <path d="M4 4v16M20 4v16" />
      <path d="M8 12h9M13 8l4 4-4 4" />
    </>
  ),
  service: (
    <>
      <circle cx="12" cy="12" r="3" />
      <path d="M12 2v4M12 18v4M2 12h4M18 12h4M5 5l3 3M16 16l3 3M19 5l-3 3M8 16l-3 3" />
    </>
  ),
  compute: (
    <>
      <rect x="3" y="4" width="18" height="7" rx="1" />
      <rect x="3" y="13" width="18" height="7" rx="1" />
      <path d="M7 7.5h.01M7 16.5h.01" />
    </>
  ),
  database: (
    <>
      <ellipse cx="12" cy="5" rx="8" ry="3" />
      <path d="M4 5v14c0 1.66 3.58 3 8 3s8-1.34 8-3V5" />
      <path d="M4 12c0 1.66 3.58 3 8 3s8-1.34 8-3" />
    </>
  ),
  storage: (
    <>
      <path d="M4 6h16l-1.5 13a2 2 0 0 1-2 1.8H7.5a2 2 0 0 1-2-1.8z" />
      <path d="M3 6h18" />
    </>
  ),
  network: (
    <>
      <circle cx="6" cy="6" r="2.4" />
      <circle cx="18" cy="6" r="2.4" />
      <circle cx="12" cy="18" r="2.4" />
      <path d="M7.6 7.8l3 7.6M16.4 7.8l-3 7.6M8.4 6h7.2" />
    </>
  ),
  certificate: (
    <>
      <circle cx="12" cy="9" r="5" />
      <path d="M8.5 13l-1.5 8 5-3 5 3-1.5-8" />
    </>
  ),
  account: (
    <>
      <path d="M4 21V5l8-3 8 3v16" />
      <path d="M4 21h16M9 9h.01M15 9h.01M9 13h.01M15 13h.01M10 21v-4h4v4" />
    </>
  ),
  workload: (
    <>
      <path d="M12 2l8 4.5v9L12 20l-8-4.5v-9z" />
      <path d="M12 2v18M12 11l8-4.5M12 11L4 6.5" />
    </>
  ),
  function: (
    <>
      <path d="M14 4h-2a3 3 0 0 0-3 3v3H6m3 0v7a3 3 0 0 1-3 3" />
      <path d="M9 12h6" />
    </>
  ),
  generic: <path d="M12 2l8.66 5v10L12 22l-8.66-5V7z" />,
};

/** The exporter's per-category colour, for callers that want the icon to carry
 *  its own family identity rather than inherit the surrounding text colour. */
export const iconTint: Record<IconKey, string> = {
  dns: '#2563eb',
  waf: '#16a34a',
  tunnel: '#9333ea',
  loadbalancer: '#db2777',
  gateway: '#4f46e5',
  service: '#0891b2',
  compute: '#475569',
  database: '#ea580c',
  storage: '#0d9488',
  network: '#0284c7',
  certificate: '#ca8a04',
  account: '#6b7280',
  workload: '#2563eb',
  function: '#7c3aed',
  generic: '#64748b',
};

/** Human label per category, for legends and tooltips. */
export const iconLabel: Record<IconKey, string> = {
  dns: 'DNS / zone',
  waf: 'Security policy',
  tunnel: 'Tunnel',
  loadbalancer: 'Load balancer',
  gateway: 'Gateway / route',
  service: 'Service / cluster',
  compute: 'Compute',
  database: 'Database',
  storage: 'Storage',
  network: 'Network',
  certificate: 'Certificate',
  account: 'Account / identity',
  workload: 'Workload',
  function: 'Function',
  generic: 'Other',
};

export interface AssetIconProps {
  /** An asset Type (`v1.Pod`, `oci.load_balancer`, …) or an explicit key. */
  type?: string;
  iconKey?: IconKey;
  size?: number;
  /** Stroke colour. Defaults to `currentColor`; pass `iconTint[key]` to use
   *  the category's own colour. */
  color?: string;
  strokeWidth?: number;
  className?: string;
  title?: string;
}

/**
 * Renders a glyph for an asset type.
 *
 * Decorative by default (`aria-hidden`), because in every place it is used the
 * type is also written out next to it — an icon that repeats adjacent text as
 * an accessible name just makes screen readers say everything twice. Pass
 * `title` for the rare standalone use and it becomes a labelled `img`.
 */
export function AssetIcon({
  type,
  iconKey,
  size = 16,
  color,
  strokeWidth = 1.7,
  className,
  title,
}: AssetIconProps) {
  const key = iconKey ?? iconKeyForType(type ?? '');
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke={color ?? 'currentColor'}
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      role={title ? 'img' : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
    >
      {title && <title>{title}</title>}
      {BODIES[key]}
    </svg>
  );
}

/**
 * Small UI glyphs that are not asset categories — used by stat tiles, toolbars
 * and empty states. Kept in the same file so there is exactly one place to
 * look for "where do icons come from".
 */
export type UIIconName =
  | 'layers'
  | 'cloud'
  | 'shapes'
  | 'building'
  | 'globe'
  | 'alert'
  | 'search'
  | 'filter'
  | 'download'
  | 'close'
  | 'copy'
  | 'check'
  | 'chevron'
  | 'play'
  | 'stop'
  | 'sun'
  | 'moon'
  | 'monitor'
  | 'zoom-in'
  | 'zoom-out'
  | 'fit'
  | 'target'
  | 'columns'
  | 'rows'
  | 'external';

const UI_BODIES: Record<UIIconName, React.ReactNode> = {
  layers: <path d="M12 3l9 5-9 5-9-5 9-5zM3 13l9 5 9-5M3 17l9 5 9-5" />,
  cloud: <path d="M7 18a4 4 0 0 1-.6-7.95A5.5 5.5 0 0 1 17 9.5a3.75 3.75 0 0 1 .3 7.49z" />,
  shapes: (
    <>
      <circle cx="7" cy="7" r="4" />
      <rect x="13" y="13" width="8" height="8" rx="1.5" />
      <path d="M17 3l4 7h-8z" />
    </>
  ),
  building: (
    <>
      <path d="M4 21V6l8-3 8 3v15" />
      <path d="M3 21h18M9 9h.01M15 9h.01M9 13h.01M15 13h.01M10 21v-4h4v4" />
    </>
  ),
  globe: (
    <>
      <circle cx="12" cy="12" r="9" />
      <path d="M3 12h18M12 3c2.6 2.7 2.6 15.3 0 18M12 3c-2.6 2.7-2.6 15.3 0 18" />
    </>
  ),
  alert: (
    <>
      <path d="M12 3.5l9.5 16.5H2.5z" />
      <path d="M12 10v4M12 17h.01" />
    </>
  ),
  search: (
    <>
      <circle cx="11" cy="11" r="6.5" />
      <path d="M16 16l4.5 4.5" />
    </>
  ),
  filter: <path d="M3 5h18l-7 8v6l-4 2v-8z" />,
  download: <path d="M12 3v12m0 0l-4.5-4.5M12 15l4.5-4.5M4 20h16" />,
  close: <path d="M6 6l12 12M18 6L6 18" />,
  copy: (
    <>
      <rect x="9" y="9" width="11" height="11" rx="2" />
      <path d="M5 15V5a2 2 0 0 1 2-2h10" />
    </>
  ),
  check: <path d="M4.5 12.5l5 5 10-11" />,
  chevron: <path d="M9 5l7 7-7 7" />,
  play: <path d="M7 4.5l12 7.5-12 7.5z" />,
  stop: <rect x="6" y="6" width="12" height="12" rx="2" />,
  sun: (
    <>
      <circle cx="12" cy="12" r="4" />
      <path d="M12 2v2M12 20v2M2 12h2M20 12h2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M19.1 4.9l-1.4 1.4M6.3 17.7l-1.4 1.4" />
    </>
  ),
  moon: <path d="M20 14.5A8.5 8.5 0 0 1 9.5 4a8.5 8.5 0 1 0 10.5 10.5z" />,
  monitor: (
    <>
      <rect x="2.5" y="4" width="19" height="12.5" rx="2" />
      <path d="M8.5 20.5h7M12 16.5v4" />
    </>
  ),
  'zoom-in': (
    <>
      <circle cx="11" cy="11" r="6.5" />
      <path d="M16 16l4.5 4.5M8.5 11h5M11 8.5v5" />
    </>
  ),
  'zoom-out': (
    <>
      <circle cx="11" cy="11" r="6.5" />
      <path d="M16 16l4.5 4.5M8.5 11h5" />
    </>
  ),
  fit: <path d="M4 9V5a1 1 0 0 1 1-1h4M15 4h4a1 1 0 0 1 1 1v4M20 15v4a1 1 0 0 1-1 1h-4M9 20H5a1 1 0 0 1-1-1v-4" />,
  target: (
    <>
      <circle cx="12" cy="12" r="7.5" />
      <circle cx="12" cy="12" r="2.5" />
      <path d="M12 1.5v3M12 19.5v3M1.5 12h3M19.5 12h3" />
    </>
  ),
  columns: (
    <>
      <rect x="3.5" y="4" width="17" height="16" rx="2" />
      <path d="M9.5 4v16M15 4v16" />
    </>
  ),
  rows: (
    <>
      <rect x="3.5" y="4" width="17" height="16" rx="2" />
      <path d="M3.5 9.5h17M3.5 15h17" />
    </>
  ),
  external: <path d="M14 4h6v6M20 4l-8.5 8.5M18 14v4a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h4" />,
};

export function Icon({
  name,
  size = 16,
  strokeWidth = 1.7,
  className,
  title,
}: {
  name: UIIconName;
  size?: number;
  strokeWidth?: number;
  className?: string;
  title?: string;
}) {
  return (
    <svg
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth={strokeWidth}
      strokeLinecap="round"
      strokeLinejoin="round"
      className={className}
      role={title ? 'img' : undefined}
      aria-hidden={title ? undefined : true}
      aria-label={title}
    >
      {title && <title>{title}</title>}
      {UI_BODIES[name]}
    </svg>
  );
}
