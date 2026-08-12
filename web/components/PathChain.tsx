'use client';

import Link from 'next/link';
import { useState } from 'react';
import { edgeColor, providerColor } from '@/lib/colors';
import { vars } from '@/lib/css';
import { AssetIcon } from '@/lib/icons';
import type { Asset, Edge, Path } from '@/lib/types';

/** Longest chain drawn whole. Past it the middle folds away: a nine-hop route
 *  wraps onto three lines and the two ends — where the question was asked and
 *  where it landed — stop being findable, which is the only part most readers
 *  scan for. */
const COLLAPSE_ABOVE = 5;
const HEAD_HOPS = 2;
const TAIL_HOPS = 2;

/**
 * One route rendered as a horizontal chain: node pills joined by connectors
 * that carry the edge's kind, port and hostname in the edge's own colour.
 *
 * Horizontal rather than the indented ladder this replaced because a route is
 * read end-to-end — "the internet reaches the pod *through* these four things"
 * — and an indent ladder makes the reader reconstruct that sequence from
 * left-edge positions. It wraps instead of scrolling so nothing is hidden
 * off-screen at 768px.
 */
export function PathChain({ path }: { path: Path }) {
  const [expanded, setExpanded] = useState(false);

  const hops = path.edges.length;
  if (path.nodes.length === 0) return null;

  const collapsible = hops > COLLAPSE_ABOVE;
  const collapsed = collapsible && !expanded;
  const hidden = collapsed ? hops - HEAD_HOPS - TAIL_HOPS : 0;

  // Built as a flat list rather than nested markup: nodes and connectors have
  // to be siblings in one wrapping flex line, or a wrapped chain breaks
  // between a connector and the node it points at.
  const items: React.ReactNode[] = [<AssetPill key="n0" asset={path.nodes[0]} />];
  const pushHop = (i: number) => {
    items.push(<Connector key={`e${i}`} from={path.nodes[i]} edge={path.edges[i]} />);
    items.push(<AssetPill key={`n${i + 1}`} asset={path.nodes[i + 1]} />);
  };

  if (collapsed) {
    for (let i = 0; i < HEAD_HOPS; i += 1) pushHop(i);
    items.push(
      <button
        key="more"
        type="button"
        className="exp-more"
        aria-expanded={false}
        aria-label={`Show ${hidden} hidden hop${hidden === 1 ? '' : 's'}`}
        onClick={() => setExpanded(true)}
      >
        … {hidden} more hop{hidden === 1 ? '' : 's'}
      </button>,
    );
    // The tail re-states the node the first shown tail hop leaves from; without
    // it the chain would jump straight from the control to an arrow with no
    // origin.
    items.push(<AssetPill key={`n${hops - TAIL_HOPS}`} asset={path.nodes[hops - TAIL_HOPS]} />);
    for (let i = hops - TAIL_HOPS; i < hops; i += 1) pushHop(i);
  } else {
    for (let i = 0; i < hops; i += 1) pushHop(i);
  }

  return (
    <div className="exp-chain">
      {items}
      {collapsible && expanded && (
        <button
          type="button"
          className="exp-more"
          aria-expanded
          aria-label="Fold the middle of this route away again"
          onClick={() => setExpanded(false)}
        >
          fold
        </button>
      )}
    </div>
  );
}

/**
 * One hop.
 *
 * The arrow points the way traffic actually flows, which is not always the way
 * the traversal walked: an upstream query ("what can reach X") walks edges
 * backwards, so for those hops the node the walk came *from* is the edge's
 * destination. Drawing a forward arrow there would state the reverse of what
 * the edge says. This mirrors `hopLine` in internal/topology/reach_render.go.
 */
function Connector({ from, edge }: { from: Asset; edge: Edge }) {
  const forward = edge.from.id === from.id && edge.from.provider === from.provider;
  const heuristic = edge.confidence === 'heuristic';
  const deny = edge.kind === 'traffic-deny';

  const label = [edge.kind, edge.port ? `:${edge.port}` : '', edge.hostname ?? '']
    .filter(Boolean)
    .join(' ');

  const line = <span className="exp-link-line" aria-hidden="true" />;
  const head = (
    <span className="exp-link-head" aria-hidden="true">
      {forward ? '▸' : '◂'}
    </span>
  );

  return (
    <span
      className="exp-link"
      data-deny={deny ? 'true' : undefined}
      style={vars({ '--link': edgeColor(edge.kind) })}
      title={
        heuristic
          ? `${label} — inferred cross-provider match, not an authoritative join`
          : label
      }
    >
      {/* Read aloud the chain still has to say which way the traffic goes; the
          arrow glyph alone is decoration to a screen reader. */}
      <span className="sr-only">
        {forward ? ' sends to, via ' : ' receives from, via '}
        {label}
        {heuristic ? ' (inferred), ' : ', '}
      </span>
      {forward ? line : head}
      <span className="exp-link-label">
        {label}
        {heuristic && <span aria-hidden="true"> ~</span>}
      </span>
      {forward ? head : line}
    </span>
  );
}

/**
 * A node in a chain, or the headline asset of a group.
 *
 * Clicking routes to the topology view focused on this node — the chain says
 * *that* a route exists, and the next question is always what else touches the
 * asset, which is a spatial question the list cannot answer. `static` drops the
 * link for the one caller that renders a pill inside a button, where a nested
 * anchor would be invalid markup.
 */
export function AssetPill({ asset, static: isStatic }: { asset: Asset; static?: boolean }) {
  const title = `${asset.name || asset.id}\n${asset.type} · ${asset.provider}${
    asset.region ? ` · ${asset.region}` : ''
  }\n${asset.id}`;

  const inner = (
    <>
      <span className="dot" style={{ background: providerColor(asset.provider) }} />
      <AssetIcon type={asset.type} size={13} />
      <strong className="truncate">{asset.name || asset.id}</strong>
    </>
  );

  if (isStatic) {
    return (
      <span className="asset-chip exp-pill" title={title}>
        {inner}
      </span>
    );
  }

  return (
    <Link
      className="asset-chip exp-pill"
      href={`/topology/?focus=${encodeURIComponent(asset.id)}`}
      title={title}
    >
      {inner}
    </Link>
  );
}
