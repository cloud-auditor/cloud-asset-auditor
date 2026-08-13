'use client';

import Link from 'next/link';
import { useEffect, useRef } from 'react';
import { useAudit } from './AuditProvider';
import { JsonView, copyText, stringify } from './JsonView';
import { providerColor, statusTone, toneColor } from '@/lib/colors';
import { BASIS_HELP, assetCost, costLabel } from '@/lib/cost';
import { AssetIcon, Icon } from '@/lib/icons';
import type { Asset } from '@/lib/types';

/** Everything a tab ring may land on inside the panel. */
const FOCUSABLE =
  'a[href], button:not([disabled]), input:not([disabled]), select, textarea, [tabindex]:not([tabindex="-1"])';

export interface AssetDrawerProps {
  asset: Asset;
  onClose: () => void;
  /** Adds a tag key to the page's search — "show me everything with this". */
  onFilterTag: (key: string) => void;
  /** Called on unmount when the element that opened the drawer is gone (its
   *  row scrolled out of the virtual window while the drawer was open). */
  returnFocus?: () => void;
}

export function AssetDrawer({ asset, onClose, onFilterTag, returnFocus }: AssetDrawerProps) {
  const { toast } = useAudit();
  const panelRef = useRef<HTMLDivElement | null>(null);

  // Held in a ref so the mount effect below can stay dependency-free: it must
  // run exactly once per open, and an inline arrow from the page would
  // re-trigger it (and re-steal focus) on every parent render.
  const returnRef = useRef(returnFocus);
  useEffect(() => {
    returnRef.current = returnFocus;
  });

  useEffect(() => {
    const opener = document.activeElement;
    panelRef.current?.focus();
    const prevOverflow = document.body.style.overflow;
    document.body.style.overflow = 'hidden';
    return () => {
      document.body.style.overflow = prevOverflow;
      if (opener instanceof HTMLElement && opener.isConnected) opener.focus();
      else returnRef.current?.();
    };
  }, []);

  const copy = (text: string, what: string) => {
    void copyText(text).then((ok) =>
      toast(
        ok
          ? { kind: 'ok', title: `${what} copied` }
          : { kind: 'warn', title: `Could not copy ${what.toLowerCase()}`, body: 'The clipboard is unavailable in this context.' },
      ),
    );
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      onClose();
      return;
    }
    if (e.key !== 'Tab') return;
    const panel = panelRef.current;
    if (!panel) return;
    const nodes = Array.from(panel.querySelectorAll<HTMLElement>(FOCUSABLE));
    if (nodes.length === 0) {
      e.preventDefault();
      return;
    }
    const first = nodes[0];
    const last = nodes[nodes.length - 1];
    const at = document.activeElement;
    if (e.shiftKey && (at === first || at === panel)) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && at === last) {
      e.preventDefault();
      first.focus();
    }
  };

  const tags = Object.entries(asset.tags ?? {}).sort(([a], [b]) => a.localeCompare(b));
  const tone = statusTone(asset.status);
  const cost = assetCost(asset);

  return (
    <>
      {/* Decorative: Esc and the header's close button are the accessible
          affordances, and announcing an empty scrim adds nothing. */}
      <div className="drawer-backdrop" aria-hidden="true" onClick={onClose} />
      <div
        ref={panelRef}
        className="drawer asset-drawer"
        role="dialog"
        aria-modal="true"
        aria-label={`Asset ${asset.name || asset.id}`}
        tabIndex={-1}
        onKeyDown={onKeyDown}
      >
        <header>
          <span className="asset-drawer-glyph" style={{ color: providerColor(asset.provider) }}>
            <AssetIcon type={asset.type} size={18} />
          </span>
          <span className="asset-drawer-title">
            <strong className="truncate" title={asset.name || asset.id}>
              {asset.name || <span className="faint">(unnamed)</span>}
            </strong>
            <span className="mono faint truncate" title={asset.type}>
              {asset.type}
            </span>
          </span>
          <span className="spacer" />
          <span className="chip" style={{ color: providerColor(asset.provider) }}>
            <span className="dot" />
            {asset.provider}
          </span>
          <button type="button" className="btn ghost icon" aria-label="Close" onClick={onClose}>
            <Icon name="close" size={15} />
          </button>
        </header>

        <div className="body">
          <button
            type="button"
            className="asset-id"
            onClick={() => copy(asset.id, 'Asset id')}
            title="Copy id"
          >
            <span className="mono truncate">{asset.id}</span>
            <Icon name="copy" size={12} />
          </button>

          <dl className="asset-fields">
            <Field label="Type" value={<span className="mono">{asset.type}</span>} />
            <Field label="Region" value={asset.region} />
            <Field label="Account" value={asset.account_id} mono />
            <Field
              label="Status"
              value={
                asset.status ? (
                  <span className="pill" style={{ color: toneColor(tone) }}>
                    <span className="dot" />
                    {asset.status}
                  </span>
                ) : undefined
              }
            />
            <Field label="Created" value={asset.created_at} />
            <Field label="Address" value={assetAddress(asset)} mono />
          </dl>

          {cost && (
            <section className="asset-section drawer-cost">
              <h3>Estimated cost</h3>
              <p className="drawer-cost-figure">
                <span className={cost.amount !== undefined ? 'mono' : 'mono faint'}>
                  {costLabel(cost)}
                </span>
                {cost.amount !== undefined && <span className="hint"> / month</span>}
                <span className="chip" style={{ marginLeft: 8 }}>
                  {cost.basis}
                </span>
              </p>
              {/* The basis is the whole point: "estimated at list price from
                  this resource's own attributes" and "no rule matched, so we
                  do not know" are very different claims, and a figure on a
                  screen erases the difference unless the UI states it. */}
              <p className="hint">{BASIS_HELP[cost.basis]}</p>
              {cost.detail && <p className="hint mono drawer-cost-detail">{cost.detail}</p>}
            </section>
          )}

          <section className="asset-section">
            <h3>
              Tags
              {tags.length > 0 && <span className="count-badge">{tags.length}</span>}
            </h3>
            {tags.length === 0 ? (
              <p className="hint">This resource carried no tags or labels.</p>
            ) : (
              <div className="asset-tags">
                {tags.map(([k, v]) => (
                  <div key={k} className="asset-tag">
                    <button
                      type="button"
                      className="asset-tag-k mono truncate"
                      title={`Search for “${k}”`}
                      onClick={() => onFilterTag(k)}
                    >
                      {k}
                    </button>
                    <button
                      type="button"
                      className="asset-tag-v mono"
                      title="Copy value"
                      onClick={() => copy(v, 'Value')}
                    >
                      {v}
                    </button>
                  </div>
                ))}
              </div>
            )}
          </section>

          <section className="asset-section">
            <h3>Raw payload</h3>
            {asset.raw === undefined || asset.raw === null ? (
              <p className="hint">
                Not collected. The provider&apos;s untouched response is only streamed when the
                server was started with <code className="mono">--include-raw</code>; without it the
                canonical fields above are all there is. The same flag is what the Kubernetes
                topology resolvers read, so a graph built from this run is thinner too.
              </p>
            ) : (
              <JsonView
                value={asset.raw}
                label={`Raw payload for ${asset.name || asset.id}`}
                onCopied={(ok) =>
                  toast(
                    ok
                      ? { kind: 'ok', title: 'Raw payload copied' }
                      : { kind: 'warn', title: 'Could not copy payload' },
                  )
                }
              />
            )}
          </section>
        </div>

        <footer>
          <button type="button" onClick={() => copy(stringify(asset), 'Asset JSON')}>
            <Icon name="copy" size={13} />
            Copy JSON
          </button>
          <Link className="btn" href={`/exposure/?from=${encodeURIComponent(asset.id)}`}>
            <Icon name="target" size={13} />
            Trace what this reaches
          </Link>
          <Link className="btn" href={`/topology/?focus=${encodeURIComponent(asset.id)}`}>
            <Icon name="shapes" size={13} />
            Show in topology
          </Link>
        </footer>
      </div>
    </>
  );
}

function Field({
  label,
  value,
  mono,
}: {
  label: string;
  value?: React.ReactNode;
  mono?: boolean;
}) {
  const empty = value === undefined || value === null || value === '';
  return (
    <>
      <dt>{label}</dt>
      <dd className={mono && !empty ? 'mono' : undefined}>
        {empty ? <span className="faint">—</span> : value}
      </dd>
    </>
  );
}

/**
 * Best-effort address for an asset.
 *
 * There is no `Asset.address` field — the canonical struct is deliberately
 * minimal (see internal/core/asset.go) — so the address has to be recovered
 * from wherever the provider put it. The key order matters: the overlay/private
 * address a resource is *reached at* is more useful in an inventory than the
 * public address it happens to egress from, so `ip` precedes `connection_ip`.
 *
 * Lives here rather than in the page so the drawer and the table cannot drift
 * on what an asset's address is; the Assets page re-exports it.
 */
export function assetAddress(a: Asset): string {
  const t = a.tags ?? {};
  for (const k of ['ip', 'ip_addresses', 'content', 'ipv6', 'connection_ip', 'cluster_ip']) {
    if (t[k]) return t[k].split(',')[0];
  }
  for (const k of ['dns_name', 'dns_label', 'hostname']) {
    if (t[k]) return t[k];
  }
  return '';
}
