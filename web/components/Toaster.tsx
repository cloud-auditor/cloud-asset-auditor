'use client';

import { useEffect, useRef, useState } from 'react';
import { useAudit } from './AuditProvider';

/** Long enough to read two lines, short enough not to sit over the table. */
const LIFETIME = 4500;
/** Matches the `toast-out` keyframe's --dur so the node leaves after it fades. */
const EXIT_MS = 220;
/** Older toasts past this are dropped from the DOM, not queued: an audit that
 *  skipped nine providers should not build a wall. */
const MAX_VISIBLE = 5;

/** Derived from the context rather than re-declared, so the shape stays in
 *  step with whatever AuditProvider decides a toast is. */
type Toast = ReturnType<typeof useAudit>['toasts'][number];

export function Toaster() {
  const { toasts, dismissToast } = useAudit();

  // The live region is rendered unconditionally, even when empty: a region
  // inserted at the same moment as its first message is frequently missed by
  // screen readers, which only watch regions that already exist.
  return (
    <div className="toast-stack" role="status" aria-live="polite" aria-atomic="false">
      {toasts
        .slice(-MAX_VISIBLE)
        // Newest first in DOM order — the stack is `column-reverse`, so this
        // puts the new toast against the bottom edge it animates up from.
        .reverse()
        .map((t, depth) => (
          <ToastItem key={String(t.id)} toast={t} depth={depth} onDismiss={() => dismissToast(t.id)} />
        ))}
    </div>
  );
}

function ToastItem({
  toast,
  depth,
  onDismiss,
}: {
  toast: Toast;
  depth: number;
  onDismiss: () => void;
}) {
  const [leaving, setLeaving] = useState(false);
  const [held, setHeld] = useState(false);
  const remaining = useRef(LIFETIME);
  const since = useRef(0);

  // onDismiss is a fresh closure on every parent render, and the parent
  // re-renders on every stream flush. Keeping it in a ref stops the exit timer
  // below from being torn down and restarted 5× a second, which would leave
  // the toast on screen forever.
  const dismiss = useRef(onDismiss);
  useEffect(() => {
    dismiss.current = onDismiss;
  });

  useEffect(() => {
    if (held || leaving) return;
    since.current = Date.now();
    const id = setTimeout(() => setLeaving(true), remaining.current);
    return () => {
      clearTimeout(id);
      remaining.current = Math.max(0, remaining.current - (Date.now() - since.current));
    };
  }, [held, leaving]);

  useEffect(() => {
    if (!leaving) return;
    const id = setTimeout(() => dismiss.current(), EXIT_MS);
    return () => clearTimeout(id);
  }, [leaving]);

  const Glyph = GLYPHS[toast.kind] ?? Info;

  return (
    <div
      className={`toast ${toast.kind}`}
      data-leaving={leaving ? 'true' : undefined}
      // Depth is expressed with margin rather than a transform: the entrance
      // and exit keyframes own `transform`, and `both` fill would overwrite it.
      style={{ marginRight: Math.min(depth, 3) * 5 }}
      onMouseEnter={() => setHeld(true)}
      onMouseLeave={() => setHeld(false)}
      onFocusCapture={() => setHeld(true)}
      onBlurCapture={() => setHeld(false)}
      onClick={() => setLeaving(true)}
    >
      {/* --rail is set by the .toast.<kind> rule; the fallback keeps the glyph
          visible rather than letting an unresolved var() void `color`. */}
      <span style={{ color: 'var(--rail, var(--accent))', display: 'flex', paddingTop: 1 }}>
        <Glyph />
      </span>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="t">{toast.title}</div>
        {toast.body && <div className="b">{toast.body}</div>}
      </div>
      {/* The card itself is click-to-dismiss; this button is what makes that
          reachable from the keyboard, so the div needs no role of its own. */}
      <button
        type="button"
        className="btn ghost icon sm"
        aria-label="Dismiss notification"
        onClick={(e) => {
          e.stopPropagation();
          setLeaving(true);
        }}
      >
        <svg width="12" height="12" viewBox="0 0 24 24" fill="none" aria-hidden="true">
          <path d="M5 5l14 14M19 5L5 19" stroke="currentColor" strokeWidth="2.2" strokeLinecap="round" />
        </svg>
      </button>
    </div>
  );
}

const GLYPHS: Record<string, () => React.ReactElement> = {
  ok: Check,
  warn: Warn,
  err: Cross,
  info: Info,
};

function Check() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="1.7" />
      <path d="m8 12.3 2.7 2.7L16 9.4" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  );
}

function Warn() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path d="M12 3.6 21 19H3l9-15.4Z" stroke="currentColor" strokeWidth="1.7" strokeLinejoin="round" />
      <path d="M12 9.4v4.2" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
      <circle cx="12" cy="16.4" r="1.05" fill="currentColor" />
    </svg>
  );
}

function Cross() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="1.7" />
      <path d="m9 9 6 6M15 9l-6 6" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
    </svg>
  );
}

function Info() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="1.7" />
      <path d="M12 11v5.4" stroke="currentColor" strokeWidth="2" strokeLinecap="round" />
      <circle cx="12" cy="7.9" r="1.05" fill="currentColor" />
    </svg>
  );
}
