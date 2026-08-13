'use client';

import { useCallback, useEffect, useId, useMemo, useRef, useState } from 'react';
import { Icon } from '@/lib/icons';
import { fmtCount } from '@/lib/format';

/**
 * A multi-select facet popover.
 *
 * Styling lives in app/assets/assets.css (`.facet*`) on top of the shared
 * `.popover` vocabulary in globals.css.
 */

export interface FacetOption {
  value: string;
  count: number;
  /** Optional mark colour — the provider facet tints its dots. */
  color?: string;
}

export interface FacetSelectProps {
  label: string;
  options: readonly FacetOption[];
  /** Empty means "no filter on this facet", not "nothing selected". */
  selected: readonly string[];
  onChange: (next: string[]) => void;
  /** Rendered before the label on the trigger. */
  glyph?: React.ReactNode;
}

export interface PopoverPlacement {
  /** True when the panel opens upwards because it would not fit below. */
  up: boolean;
  /** Which edge the panel is pinned to. */
  align: 'start' | 'end';
}

/**
 * Dismissal + placement for a trigger/panel pair.
 *
 * Placement is measured in a plain effect rather than useLayoutEffect: this app
 * is prerendered by `next build`, where useLayoutEffect logs a warning on every
 * render, and the panel fades in over 200ms — a correction on the next frame
 * lands inside the entrance and is invisible.
 */
export function usePopover(open: boolean, close: () => void) {
  const wrapRef = useRef<HTMLDivElement | null>(null);
  const panelRef = useRef<HTMLDivElement | null>(null);
  const [placement, setPlacement] = useState<PopoverPlacement>({ up: false, align: 'start' });

  useEffect(() => {
    if (!open) return;
    const wrap = wrapRef.current;
    const panel = panelRef.current;
    if (!wrap || !panel) return;
    const t = wrap.getBoundingClientRect();
    const p = panel.getBoundingClientRect();
    const margin = 12;
    setPlacement({
      // Flip up only when there is genuinely more room above; a panel that is
      // too tall for either side stays below, where its own scroll works.
      up: t.bottom + p.height + margin > window.innerHeight && t.top - p.height - margin > 0,
      align: t.left + p.width + margin > window.innerWidth ? 'end' : 'start',
    });
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const onDown = (e: PointerEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) close();
    };
    // pointerdown, not click: a click that starts inside and ends outside (a
    // drag across the list) should not read as an outside click.
    document.addEventListener('pointerdown', onDown, true);
    return () => document.removeEventListener('pointerdown', onDown, true);
  }, [open, close]);

  return { wrapRef, panelRef, placement };
}

export function FacetSelect({ label, options, selected, onChange, glyph }: FacetSelectProps) {
  const [open, setOpen] = useState(false);
  const [query, setQuery] = useState('');
  const [active, setActive] = useState(0);

  const triggerRef = useRef<HTMLButtonElement | null>(null);
  const listRef = useRef<HTMLDivElement | null>(null);
  // useId's value carries delimiters that are illegal at the head of a CSS
  // selector, and these ids are only ever referenced from ARIA attributes — so
  // strip everything a selector would choke on and keep it stable.
  const id = `facet${useId().replace(/[^a-zA-Z0-9_-]/g, '')}`;

  // Outside clicks close without pulling focus back: the click has already
  // moved focus somewhere the operator chose, and yanking it to the trigger
  // would undo their own action. Esc, which has no other target, does restore.
  const close = useCallback(() => setOpen(false), []);
  const { wrapRef, panelRef, placement } = usePopover(open, close);

  const sel = useMemo(() => new Set(selected), [selected]);

  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    return q ? options.filter((o) => o.value.toLowerCase().includes(q)) : options;
  }, [options, query]);

  useEffect(() => {
    if (!open) return;
    setQuery('');
    setActive(0);
  }, [open]);

  useEffect(() => {
    if (!open) return;
    listRef.current?.querySelector(`[data-idx="${active}"]`)?.scrollIntoView({ block: 'nearest' });
  }, [open, active]);

  const dismiss = (restoreFocus: boolean) => {
    setOpen(false);
    if (restoreFocus) triggerRef.current?.focus();
  };

  const toggle = (value: string) => {
    onChange(sel.has(value) ? selected.filter((v) => v !== value) : selected.concat(value));
  };

  const onKeyDown = (e: React.KeyboardEvent) => {
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        dismiss(true);
        break;
      case 'ArrowDown':
        e.preventDefault();
        setActive((i) => (shown.length === 0 ? 0 : (i + 1) % shown.length));
        break;
      case 'ArrowUp':
        e.preventDefault();
        setActive((i) => (shown.length === 0 ? 0 : (i - 1 + shown.length) % shown.length));
        break;
      case 'Home':
        e.preventDefault();
        setActive(0);
        break;
      case 'End':
        e.preventDefault();
        setActive(Math.max(0, shown.length - 1));
        break;
      case 'Enter': {
        e.preventDefault();
        const hit = shown[active];
        if (hit) toggle(hit.value);
        break;
      }
      case 'Tab':
        // One focusable control by design — the list is a listbox driven by
        // aria-activedescendant, exactly like the command palette — so the trap
        // is just refusing to leave the search input.
        e.preventDefault();
        break;
    }
  };

  const n = selected.length;

  return (
    <div className="facet" ref={wrapRef}>
      <button
        ref={triggerRef}
        type="button"
        className="btn ghost facet-trigger"
        data-on={n > 0 ? 'true' : undefined}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? dismiss(true) : setOpen(true))}
      >
        {glyph}
        {label}
        {n > 0 && <span className="count-badge accent">{n}</span>}
        <Icon name="chevron" size={11} className="facet-caret" />
      </button>

      {open && (
        <div
          ref={panelRef}
          className="popover facet-pop"
          data-up={placement.up ? 'true' : undefined}
          data-align={placement.align}
          onKeyDown={onKeyDown}
        >
          <div className="facet-search">
            <Icon name="search" size={13} />
            <input
              autoFocus
              value={query}
              onChange={(e) => {
                setQuery(e.target.value);
                setActive(0);
              }}
              placeholder={`Filter ${label.toLowerCase()}…`}
              aria-label={`Filter ${label} values`}
              role="combobox"
              aria-expanded="true"
              aria-autocomplete="list"
              aria-controls={`${id}-list`}
              aria-activedescendant={shown.length > 0 ? `${id}-opt-${active}` : undefined}
              autoComplete="off"
              spellCheck={false}
            />
          </div>

          <div className="facet-actions">
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => onChange(shown.map((o) => o.value))}
              disabled={shown.length === 0}
            >
              All
            </button>
            <button
              type="button"
              className="btn ghost sm"
              onClick={() => onChange([])}
              disabled={n === 0}
            >
              None
            </button>
            <span className="spacer" />
            <span className="hint">
              {shown.length} value{shown.length === 1 ? '' : 's'}
            </span>
          </div>

          <div
            className="facet-list"
            id={`${id}-list`}
            role="listbox"
            aria-multiselectable="true"
            aria-label={label}
            ref={listRef}
          >
            {shown.length === 0 && <p className="hint facet-none">No matching values.</p>}
            {shown.map((o, i) => {
              const on = sel.has(o.value);
              return (
                <div
                  key={o.value}
                  id={`${id}-opt-${i}`}
                  data-idx={i}
                  role="option"
                  aria-selected={on}
                  className="item facet-item"
                  data-active={i === active ? 'true' : undefined}
                  onClick={() => toggle(o.value)}
                  onMouseMove={() => setActive(i)}
                >
                  <span className="facet-box" data-on={on ? 'true' : undefined} aria-hidden="true">
                    {on && <Icon name="check" size={10} strokeWidth={2.6} />}
                  </span>
                  {o.color && <span className="dot" style={{ background: o.color }} />}
                  <span className="truncate">{o.value}</span>
                  <span className="spacer" />
                  <span className="mono faint">{fmtCount(o.count)}</span>
                </div>
              );
            })}
          </div>
        </div>
      )}
    </div>
  );
}
