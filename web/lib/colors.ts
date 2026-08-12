import { EDGE_KINDS } from './types';

/**
 * One colour per provider, resolved from the CSS custom properties in
 * globals.css so light/dark theming stays in one place. An unknown provider
 * falls back to the neutral "multi" accent rather than a random hue — a
 * stable grey reads as "unclassified", a random hue reads as meaningful.
 */
const PROVIDER_VARS: Record<string, string> = {
  cloudflare: '--p-cloudflare',
  oci: '--p-oci',
  kubernetes: '--p-kubernetes',
  gcp: '--p-gcp',
  netbird: '--p-netbird',
  tailscale: '--p-tailscale',
  demo: '--p-multi',
  multi: '--p-multi',
};

export function providerColor(provider: string): string {
  return `var(${PROVIDER_VARS[provider] ?? '--p-multi'})`;
}

/**
 * A soft, provider-tinted fill for surfaces (rails, chips, chart segments in
 * their hover state). `color-mix` keeps the tint honest in both themes: mixing
 * toward `transparent` preserves the hue over whatever surface is behind it,
 * where a hard-coded rgba() would be tuned for exactly one background.
 */
export function providerFill(provider: string, percent = 14): string {
  return `color-mix(in srgb, ${providerColor(provider)} ${percent}%, transparent)`;
}

/** Two-stop gradient for a provider, used by bars and donut segments. */
export function providerGradient(provider: string): string {
  const c = providerColor(provider);
  return `linear-gradient(90deg, ${c}, color-mix(in srgb, ${c} 55%, var(--accent-2)))`;
}

export const PROVIDERS_WITH_COLOR = Object.keys(PROVIDER_VARS).filter(
  (p) => p !== 'multi' && p !== 'demo',
);

/**
 * Edge colours. Traffic-flow edges are coloured by verdict — a denial drawn
 * like a grant reads as reachability, the exact opposite of what the rule
 * says. This mirrors `edgeColor` in internal/topology/render.go so the
 * in-browser diagram and the exported Graphviz/Mermaid ones agree.
 */
export function edgeColor(kind: string): string {
  switch (kind) {
    case EDGE_KINDS.trafficAllow:
      return 'var(--success)';
    case EDGE_KINDS.trafficDeny:
      return 'var(--danger)';
    case EDGE_KINDS.dns:
      return 'var(--accent)';
    case EDGE_KINDS.waf:
      return 'var(--warn)';
    case EDGE_KINDS.gatewayRoute:
      return 'var(--info)';
    case EDGE_KINDS.serviceBackend:
      return 'var(--accent-2)';
    default:
      return 'var(--text-faint)';
  }
}

/** Human labels for the edge kinds, used by the legend and the inspector. */
export const EDGE_LABELS: Record<string, string> = {
  [EDGE_KINDS.dns]: 'DNS → target',
  [EDGE_KINDS.waf]: 'Security rule → zone',
  [EDGE_KINDS.lbBackend]: 'Load balancer → backend',
  [EDGE_KINDS.gatewayRoute]: 'Gateway → service',
  [EDGE_KINDS.serviceBackend]: 'Service → workload',
  [EDGE_KINDS.networkContainment]: 'Contained in network',
  [EDGE_KINDS.trafficAllow]: 'Traffic allowed',
  [EDGE_KINDS.trafficDeny]: 'Traffic denied',
};

/** A one-line explanation of what each edge kind is derived from, for the
 *  legend's tooltips — the labels alone don't say where the claim came from,
 *  and provenance is what tells an operator how much to trust an edge. */
export const EDGE_SOURCES: Record<string, string> = {
  [EDGE_KINDS.dns]: 'A DNS record whose value matches a collected IP or hostname.',
  [EDGE_KINDS.waf]: 'A Cloudflare ruleset, Access app, or page rule bound to a zone.',
  [EDGE_KINDS.lbBackend]: 'A load balancer address matching a cluster service address.',
  [EDGE_KINDS.gatewayRoute]: 'An Ingress or HTTPRoute spec naming a backend Service.',
  [EDGE_KINDS.serviceBackend]: 'A Service selector matching Pod labels in the same namespace.',
  [EDGE_KINDS.networkContainment]: 'An OCID tag placing a resource inside a VCN or subnet.',
  [EDGE_KINDS.trafficAllow]: 'A NetworkPolicy, Tailscale ACL, or NetBird policy that permits traffic.',
  [EDGE_KINDS.trafficDeny]: 'A policy rule that denies traffic. It is not a path — it is the absence of one.',
};

export type Tone = 'ok' | 'warn' | 'err' | 'info' | 'neutral';

/**
 * Maps a provider's free-text `status` onto one of the semantic tones.
 *
 * Every provider spells health differently ("RUNNING", "Active", "healthy",
 * "Succeeded", "ACTIVE"), and a status string is not a closed set — a new
 * resource type can introduce a new word at any time. So this matches on
 * substrings and, crucially, returns `neutral` rather than guessing when
 * nothing matches: colouring an unknown status green would assert health the
 * inventory never claimed.
 */
export function statusTone(status: string | undefined): Tone {
  if (!status) return 'neutral';
  const s = status.toLowerCase();
  if (/fail|error|terminat|deleted|unhealthy|crash|denied|expired|revoked/.test(s)) return 'err';
  if (/pending|provision|creating|updating|progress|degraded|warn|unknown|paused|suspend/.test(s)) {
    return 'warn';
  }
  if (/run|active|available|ready|healthy|succeed|bound|enabled|online|true/.test(s)) return 'ok';
  return 'neutral';
}

/** The CSS colour for a tone. Kept next to statusTone so the two never drift. */
export function toneColor(tone: Tone): string {
  switch (tone) {
    case 'ok':
      return 'var(--success)';
    case 'warn':
      return 'var(--warn)';
    case 'err':
      return 'var(--danger)';
    case 'info':
      return 'var(--info)';
    default:
      return 'var(--text-dim)';
  }
}
