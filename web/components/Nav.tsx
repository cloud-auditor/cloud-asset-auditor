'use client';

import Link from 'next/link';
import { usePathname } from 'next/navigation';
import { useEffect, useRef, useState } from 'react';
import { useAudit } from './AuditProvider';
import { ThemeToggle } from './ThemeToggle';
import { fetchProviders } from '@/lib/api';
import { fmtCount } from '@/lib/format';

const LINKS = [
  { href: '/', label: 'Dashboard' },
  { href: '/assets/', label: 'Assets' },
  { href: '/topology/', label: 'Topology' },
  { href: '/exposure/', label: 'Exposure' },
];

/** Trailing slashes come from next.config's `trailingSlash: true`; compare
 *  against both forms so "/assets" and "/assets/" both light up. */
function isActive(pathname: string, href: string): boolean {
  return pathname === href || pathname === href.replace(/\/$/, '');
}

export function Nav() {
  const pathname = usePathname();
  const { running, assets, elapsedMs, startedAt, failure, errors } = useAudit();
  const [authMode, setAuthMode] = useState('');
  const [modKey, setModKey] = useState('⌘');

  const navRef = useRef<HTMLElement | null>(null);
  const linkRefs = useRef(new Map<string, HTMLAnchorElement>());
  const [thumb, setThumb] = useState<{ x: number; w: number } | null>(null);

  const activeHref = LINKS.find((l) => isActive(pathname, l.href))?.href;

  useEffect(() => {
    const ctl = new AbortController();
    // Purely informational — a failure here (server down, auth challenge)
    // must not stop the rest of the app rendering.
    fetchProviders(ctl.signal)
      .then((r) => setAuthMode(r.auth_mode))
      .catch(() => {});
    return () => ctl.abort();
  }, []);

  useEffect(() => {
    // Set after mount rather than during render: the value differs per machine
    // and the export is prerendered, so reading it during render would make the
    // markup disagree with the hydrated tree.
    if (!/Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent)) setModKey('Ctrl');
  }, []);

  // Measure the active link and drive the sliding indicator from it. offsetLeft
  // is relative to the offsetParent's padding edge, which is the same origin
  // `left: 0` resolves against, so the two agree without a rect subtraction.
  // Until this lands `thumb` is null, the `.thumb` element is absent, and
  // globals.css falls back to the active link painting its own pill.
  useEffect(() => {
    const nav = navRef.current;
    const el = activeHref ? linkRefs.current.get(activeHref) : undefined;
    if (!nav || !el) {
      setThumb(null);
      return;
    }
    const measure = () => setThumb({ x: el.offsetLeft, w: el.offsetWidth });
    measure();
    // Fonts finish loading and the topbar reflows below 720px; both change the
    // geometry without changing the route.
    const ro = new ResizeObserver(measure);
    ro.observe(nav);
    ro.observe(el);
    return () => ro.disconnect();
  }, [activeHref]);

  const status = failure
    ? { cls: 'err', text: 'failed' }
    : running
      ? { cls: 'live', text: 'streaming' }
      : elapsedMs != null
        ? errors.length > 0
          ? { cls: 'warn', text: `${errors.length} error${errors.length === 1 ? '' : 's'}` }
          : { cls: 'ok', text: 'complete' }
        : { cls: '', text: 'idle' };

  return (
    <header className="topbar">
      <span className="brand">
        <span className="mark">
          <OrbitMark />
        </span>
        Cloud Asset Auditor
      </span>

      <nav className="nav segmented" aria-label="Primary" ref={navRef}>
        {thumb && (
          <span
            className="thumb"
            aria-hidden="true"
            style={{ '--seg-x': `${thumb.x}px`, '--seg-w': `${thumb.w}px` } as React.CSSProperties}
          />
        )}
        {LINKS.map((l) => {
          const on = isActive(pathname, l.href);
          return (
            <Link
              key={l.href}
              href={l.href}
              className="btn"
              // data-on is what .segmented keys off. There is deliberately no
              // second data-active flag: two attributes meaning "this one" is
              // how a stylesheet ends up with two disagreeing active states.
              data-on={on ? 'true' : 'false'}
              aria-current={on ? 'page' : undefined}
              ref={(el) => {
                if (el) linkRefs.current.set(l.href, el);
                else linkRefs.current.delete(l.href);
              }}
            >
              {l.label}
            </Link>
          );
        })}
      </nav>

      <div className="topbar-right">
        <span className={`pill${status.cls ? ` ${status.cls}` : ''}`}>
          {running ? <span className="pulse-dot" /> : <span className="dot" />}
          {status.text}
        </span>

        <span className="dim" style={{ display: 'inline-flex', alignItems: 'baseline', gap: 4 }}>
          <Ticker value={assets.length} live={running} />
          assets
        </span>

        <Elapsed running={running} startedAt={startedAt} elapsedMs={elapsedMs} />

        <ThemeToggle />

        <button
          type="button"
          className="btn ghost sm"
          style={{ gap: 3 }}
          aria-label="Open command palette"
          title="Command palette"
          onClick={() => window.dispatchEvent(new Event('auditor:palette'))}
        >
          <span className="kbd">{modKey}</span>
          <span className="kbd">K</span>
        </button>

        {authMode && authMode !== 'none' && <span className="chip">auth: {authMode}</span>}
        <a className="btn ghost sm" href="/api/v1/openapi.yaml" target="_blank" rel="noreferrer">
          API
        </a>
      </div>

      {running && <div className="progress-line" />}
    </header>
  );
}

/**
 * Counts from the previously *rendered* value to the new one, so a stream
 * flushing every 200ms reads as a rising number rather than a stuttering one.
 * Animating from the rendered value (not the previous prop) means an
 * interrupted run picks up exactly where the eye left it.
 */
export function Ticker({ value, live }: { value: number; live?: boolean }) {
  const [display, setDisplay] = useState(value);
  const displayRef = useRef(value);

  useEffect(() => {
    const from = displayRef.current;
    if (from === value) return;

    if (window.matchMedia('(prefers-reduced-motion: reduce)').matches) {
      displayRef.current = value;
      setDisplay(value);
      return;
    }

    const t0 = performance.now();
    let raf = 0;
    const step = (now: number) => {
      // Clamped at BOTH ends. A rAF callback is handed the frame's start
      // time, which can predate the performance.now() sampled when this
      // effect ran — so `now - t0` is negative on the first frame more often
      // than it looks. Unclamped, the cubic ease turns that into a large
      // negative multiplier and the counter paints a nonsense number, which
      // then becomes the next animation's starting value.
      const p = Math.min(1, Math.max(0, (now - t0) / 400));
      const eased = 1 - Math.pow(1 - p, 3);
      const v = Math.round(from + (value - from) * eased);
      displayRef.current = v;
      setDisplay(v);
      if (p < 1) raf = requestAnimationFrame(step);
    };
    raf = requestAnimationFrame(step);
    return () => cancelAnimationFrame(raf);
  }, [value]);

  return (
    <span className="tick" data-live={live ? 'true' : undefined}>
      {fmtCount(display)}
    </span>
  );
}

function Elapsed({
  running,
  startedAt,
  elapsedMs,
}: {
  running: boolean;
  startedAt: string | null;
  elapsedMs: number | null;
}) {
  const [live, setLive] = useState(0);

  useEffect(() => {
    if (!running) return;
    // started_at is the *server's* clock. If it is ahead of the browser's the
    // difference would render as a negative age, so fall back to local time
    // whenever the two disagree in the impossible direction.
    const parsed = startedAt ? Date.parse(startedAt) : NaN;
    const t0 = Number.isFinite(parsed) && parsed <= Date.now() ? parsed : Date.now();
    const tick = () => setLive(Date.now() - t0);
    tick();
    const id = setInterval(tick, 100);
    return () => clearInterval(id);
  }, [running, startedAt]);

  const ms = running ? live : elapsedMs;
  if (ms == null) return null;
  return (
    <span className="tick dim" title={running ? 'Elapsed' : 'Run duration'}>
      {formatMs(ms)}
    </span>
  );
}

function formatMs(ms: number): string {
  if (ms < 1000) return `${Math.max(0, Math.round(ms))}ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(1)}s`;
  const m = Math.floor(ms / 60_000);
  return `${m}m ${String(Math.floor((ms % 60_000) / 1000)).padStart(2, '0')}s`;
}

/** Inline mark — an external logo file would be one more embedded asset and
 *  one more request; this is a few hundred bytes in the HTML. Two crossed
 *  orbits around a solid core: an inventory is things circling a centre, and
 *  it stays legible at 16px where a wire globe turns into a grey smudge.
 *  Strokes use currentColor so the gradient tile's --accent-ink drives it. */
function OrbitMark() {
  return (
    <svg width="16" height="16" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <ellipse
        cx="12"
        cy="12"
        rx="10"
        ry="4.6"
        stroke="currentColor"
        strokeWidth="1.7"
        transform="rotate(-28 12 12)"
      />
      <ellipse
        cx="12"
        cy="12"
        rx="10"
        ry="4.6"
        stroke="currentColor"
        strokeWidth="1.7"
        opacity="0.55"
        transform="rotate(30 12 12)"
      />
      <circle cx="12" cy="12" r="3.1" fill="currentColor" />
      <circle cx="20.8" cy="7.3" r="1.5" fill="currentColor" />
    </svg>
  );
}
