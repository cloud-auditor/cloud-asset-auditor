'use client';

import { useState } from 'react';
import { Icon } from '@/lib/icons';
import { fmtCount } from '@/lib/format';

/**
 * A collapsible viewer for an asset's `raw` payload.
 *
 * The payload is a Kubernetes object as often as not, which means a few hundred
 * keys, a `managedFields` array nobody wants expanded, and enough nesting that
 * a naive full render costs more frame budget than the rest of the drawer put
 * together. Three caps keep it honest:
 *
 *  - Nodes below OPEN_DEPTH start collapsed, and a collapsed node renders no
 *    children at all — so the recursion depth (and the DOM) is bounded by what
 *    the reader has actually opened, not by the document.
 *  - Containers render at most CHUNK children with a "… N more" expander. This
 *    applies to objects as well as arrays: a ConfigMap with 500 keys is exactly
 *    as expensive as a 500-element list.
 *  - MAX_DEPTH is a backstop against a pathologically deep document; nothing
 *    real reaches it, but the render is recursive and the stack is finite.
 *
 * Token classes (`.json-key`, `.json-str`, …) and the indent guides come from
 * globals.css; the block chrome is in app/assets/assets.css.
 */

const OPEN_DEPTH = 2;
const CHUNK = 50;
const MAX_DEPTH = 20;

type Kind = 'object' | 'array' | 'string' | 'number' | 'boolean' | 'null';

function kindOf(v: unknown): Kind {
  if (v === null || v === undefined) return 'null';
  if (Array.isArray(v)) return 'array';
  switch (typeof v) {
    case 'object':
      return 'object';
    case 'number':
    case 'bigint':
      return 'number';
    case 'boolean':
      return 'boolean';
    default:
      return 'string';
  }
}

export interface JsonViewProps {
  value: unknown;
  /** Announced to assistive tech; the visible label is the caller's business. */
  label?: string;
  onCopied?: (ok: boolean) => void;
}

export function JsonView({ value, label = 'Raw payload', onCopied }: JsonViewProps) {
  return (
    <div className="json-block">
      <button
        type="button"
        className="btn ghost icon sm json-copy"
        aria-label="Copy raw JSON"
        title="Copy raw JSON"
        onClick={() => {
          void copyText(stringify(value)).then((ok) => onCopied?.(ok));
        }}
      >
        <Icon name="copy" size={13} />
      </button>
      <div className="json-view" role="group" aria-label={label} tabIndex={0}>
        <JsonNode value={value} depth={0} last />
      </div>
    </div>
  );
}

function JsonNode({
  name,
  value,
  depth,
  last,
}: {
  name?: string;
  value: unknown;
  depth: number;
  last: boolean;
}) {
  const kind = kindOf(value);
  const [open, setOpen] = useState(depth < OPEN_DEPTH);
  const [shown, setShown] = useState(CHUNK);

  const pad = { paddingLeft: `${depth * 2}ch` };
  const key = name === undefined ? null : <span className="json-key">{JSON.stringify(name)}: </span>;
  const comma = last ? '' : ',';

  if (kind !== 'object' && kind !== 'array') {
    return (
      <div className="json-row" style={pad}>
        {key}
        <Scalar value={value} kind={kind} />
        {comma}
      </div>
    );
  }

  if (depth >= MAX_DEPTH) {
    return (
      <div className="json-row" style={pad}>
        {key}
        <span className="json-null">…{comma}</span>
      </div>
    );
  }

  const entries: [string | undefined, unknown][] = Array.isArray(value)
    ? value.map((v) => [undefined, v] as [undefined, unknown])
    : Object.entries(value as Record<string, unknown>);
  const [openBrace, closeBrace] = Array.isArray(value) ? ['[', ']'] : ['{', '}'];
  const n = entries.length;

  if (n === 0) {
    return (
      <div className="json-row" style={pad}>
        {key}
        {openBrace}
        {closeBrace}
        {comma}
      </div>
    );
  }

  const visible = open ? entries.slice(0, shown) : [];

  return (
    <>
      <div className="json-row" style={pad}>
        <button
          type="button"
          className="json-toggle"
          aria-expanded={open}
          onClick={() => setOpen(!open)}
        >
          {/* The rotation hangs on a wrapper because Icon exposes className,
              not style or data attributes. */}
          <span className="json-caret" data-open={open ? 'true' : undefined}>
            <Icon name="chevron" size={9} />
          </span>
          {key}
          {openBrace}
        </button>
        {!open && (
          <span className="json-fold">
            {' '}
            … {n} {Array.isArray(value) ? (n === 1 ? 'item' : 'items') : n === 1 ? 'key' : 'keys'}{' '}
            {closeBrace}
            {comma}
          </span>
        )}
      </div>

      {open && (
        <>
          {visible.map(([k, v], i) => (
            <JsonNode
              key={k ?? i}
              name={k}
              value={v}
              depth={depth + 1}
              last={i === n - 1}
            />
          ))}
          {n > shown && (
            <div className="json-row" style={{ paddingLeft: `${(depth + 1) * 2}ch` }}>
              <button
                type="button"
                className="json-more"
                onClick={() => setShown((s) => Math.min(n, s + CHUNK * 4))}
              >
                … {fmtCount(n - shown)} more
              </button>
            </div>
          )}
          <div className="json-row" style={pad}>
            {closeBrace}
            {comma}
          </div>
        </>
      )}
    </>
  );
}

function Scalar({ value, kind }: { value: unknown; kind: Kind }) {
  switch (kind) {
    case 'string':
      return <span className="json-str">{JSON.stringify(String(value))}</span>;
    case 'number':
      return <span className="json-num">{String(value)}</span>;
    case 'boolean':
      return <span className="json-bool">{String(value)}</span>;
    default:
      return <span className="json-null">null</span>;
  }
}

/** Never throws: a payload with a cycle (which the API cannot produce, but a
 *  future caller might hand us) must degrade to a message, not a blank drawer. */
export function stringify(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2) ?? 'null';
  } catch {
    return '/* payload could not be serialised */';
  }
}

/**
 * Copies text, reporting success so the caller can raise the right toast.
 *
 * The async clipboard API is undefined outside a secure context, and this UI is
 * routinely reached over a plain-HTTP `kubectl port-forward` — so the
 * deprecated execCommand path is the one that actually runs for many operators,
 * not a legacy-browser courtesy.
 */
export async function copyText(text: string): Promise<boolean> {
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    // Permission denied or a non-secure context: fall through to the fallback.
  }
  try {
    const ta = document.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.style.position = 'fixed';
    ta.style.top = '0';
    ta.style.opacity = '0';
    document.body.appendChild(ta);
    ta.select();
    const ok = document.execCommand('copy');
    ta.remove();
    return ok;
  } catch {
    return false;
  }
}
