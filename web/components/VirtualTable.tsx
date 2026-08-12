'use client';

import { useCallback, useEffect, useImperativeHandle, useRef, useState } from 'react';
import { vars } from '@/lib/css';

/**
 * A windowed table.
 *
 * Why hand-rolled: an inventory can hold 50k rows and the bundle is embedded in
 * the Go binary, so a virtualization dependency is not on the table (CLAUDE.md,
 * mistake 4). The whole thing is three ideas:
 *
 *  1. **Fixed row height.** Every offset is `index * rowHeight`, so the scroll
 *     position → index mapping is arithmetic. Measuring rows would mean a
 *     layout read per row per frame, which is exactly the cost being avoided.
 *  2. **Render the slice, not the list.** The spacer is `count * rowHeight`
 *     tall so the scrollbar is honest; only `viewport / rowHeight + 2·overscan`
 *     rows exist in the DOM. At 50k rows that is ~40 elements, not 50,000.
 *  3. **Re-render on index change, not on scroll.** Rows are absolutely
 *     positioned inside the spacer, so they move with the scroll for free —
 *     the compositor does it. State only changes when the *first visible index*
 *     changes, i.e. once per `rowHeight` pixels rather than once per scroll
 *     event, and React bails out of the identical `setFirst` in between.
 *
 * The remaining budget is whatever `renderRow` costs times ~40, which is the
 * consumer's problem to keep cheap. The spacer's own height is the one hard
 * ceiling: browsers clamp an element somewhere past 17M px, which at a 40px row
 * is ~400k rows — an order of magnitude beyond the largest inventory this tool
 * has to draw, so no scroll-offset remapping is warranted here.
 *
 * Class vocabulary (`.vtable`, `.vhead`, `.vtable-spacer`, `.vrow`, `--cols`)
 * lives in app/globals.css.
 */

export interface VirtualColumn {
  key: string;
  label: string;
  /** One grid track, spliced into `--cols` — e.g. `minmax(120px, 1fr)`. */
  width: string;
  sortable?: boolean;
}

export interface VirtualTableHandle {
  /** Moves the roving focus to a row, scrolling it into view first. */
  focusRow: (index: number) => void;
}

export interface VirtualTableProps<T> {
  rows: readonly T[];
  columns: readonly VirtualColumn[];
  /** Stable identity for a row. Also what `selectedKey` is compared against. */
  rowKey: (row: T, index: number) => string;
  /**
   * The row's cells, in `columns` order. Each returned element becomes a grid
   * item of the `.vrow`, so it must carry `role="gridcell"` itself — this
   * component never wraps them, which is what keeps it generic.
   */
  renderRow: (row: T, index: number) => React.ReactNode;
  /** Must match the `--row-h` the rows are laid out with; it is set inline. */
  rowHeight: number;
  /** Accessible name for the grid. */
  label: string;
  sortKey?: string;
  sortDesc?: boolean;
  onSort?: (key: string) => void;
  onActivate?: (row: T, index: number) => void;
  rowProps?: (row: T, index: number) => { className?: string; style?: React.CSSProperties } | undefined;
  selectedKey?: string | null;
  overscan?: number;
  /** Changing this scrolls back to the top and drops the focused row — a new
   *  filter is a new list, and keeping the old offset lands nowhere useful. */
  resetKey?: string;
  handleRef?: React.RefObject<VirtualTableHandle | null>;
  className?: string;
  style?: React.CSSProperties;
  /** Shown in place of the rows when there are none. The header stays. */
  empty?: React.ReactNode;
}


export function VirtualTable<T>({
  rows,
  columns,
  rowKey,
  renderRow,
  rowHeight,
  label,
  sortKey,
  sortDesc,
  onSort,
  onActivate,
  rowProps,
  selectedKey,
  overscan = 8,
  resetKey,
  handleRef,
  className,
  style,
  empty,
}: VirtualTableProps<T>) {
  const scrollRef = useRef<HTMLDivElement | null>(null);
  const rowEls = useRef(new Map<number, HTMLDivElement>());
  const rafRef = useRef(0);
  // The focused index is mirrored in a ref because the focus effect below runs
  // off a tick counter and must read the value the handler just set, not the
  // one captured when the effect was created.
  const focusRef = useRef<number | null>(null);

  const [viewportH, setViewportH] = useState(0);
  const [first, setFirst] = useState(0);
  const [focus, setFocus] = useState<number | null>(null);
  const [focusTick, setFocusTick] = useState(0);

  const count = rows.length;
  const perScreen = Math.max(1, Math.ceil(viewportH / rowHeight));
  const firstIdx = Math.min(first, Math.max(0, count - 1));
  const startIdx = Math.max(0, firstIdx - overscan);
  const endIdx = Math.min(count, firstIdx + perScreen + 1 + overscan);

  const cols = columns.map((c) => c.width).join(' ');

  const syncFirst = useCallback(() => {
    const el = scrollRef.current;
    if (!el) return;
    // Identical value → React bails out without re-rendering, which is what
    // makes sub-row-height scrolling free.
    setFirst(Math.max(0, Math.floor(el.scrollTop / rowHeight)));
  }, [rowHeight]);

  const onScroll = useCallback(() => {
    // A fast wheel fires several scroll events per frame; one measurement per
    // frame is all that can matter, and it keeps the layout read out of the
    // event handler's critical path.
    if (rafRef.current) return;
    rafRef.current = window.requestAnimationFrame(() => {
      rafRef.current = 0;
      syncFirst();
    });
  }, [syncFirst]);

  useEffect(() => () => window.cancelAnimationFrame(rafRef.current), []);

  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    setViewportH(el.clientHeight);
    // The container resizes without the window resizing: the toolbar above it
    // wraps to a second line, the audit bar collapses after a run, and the
    // drawer opening changes nothing but a phone rotation does.
    const ro = new ResizeObserver(() => {
      setViewportH(el.clientHeight);
      syncFirst();
    });
    ro.observe(el);
    return () => ro.disconnect();
  }, [syncFirst]);

  useEffect(() => {
    const el = scrollRef.current;
    if (el) el.scrollTop = 0;
    setFirst(0);
    setFocus(null);
    focusRef.current = null;
  }, [resetKey]);

  /** Scrolls `i` into view *synchronously*, so the row exists in the DOM by the
   *  time the focus effect runs — a scroll event would land a frame too late. */
  const ensureVisible = useCallback(
    (i: number) => {
      const el = scrollRef.current;
      if (!el) return;
      const top = i * rowHeight;
      if (top < el.scrollTop) el.scrollTop = top;
      else if (top + rowHeight > el.scrollTop + el.clientHeight) {
        el.scrollTop = top + rowHeight - el.clientHeight;
      }
      syncFirst();
    },
    [rowHeight, syncFirst],
  );

  const moveFocus = useCallback(
    (to: number) => {
      if (count === 0) return;
      const i = Math.max(0, Math.min(count - 1, to));
      focusRef.current = i;
      setFocus(i);
      ensureVisible(i);
      setFocusTick((t) => t + 1);
    },
    [count, ensureVisible],
  );

  // Only ever fires in response to an explicit request (a key, a click, the
  // imperative handle). Keying it on the index instead would re-steal focus
  // every time a scrolled-away row wandered back into the window.
  useEffect(() => {
    const i = focusRef.current;
    if (i == null) return;
    rowEls.current.get(i)?.focus();
  }, [focusTick]);

  useImperativeHandle(handleRef, () => ({ focusRow: moveFocus }), [moveFocus]);

  const onKeyDown = (e: React.KeyboardEvent<HTMLDivElement>) => {
    if (count === 0) return;
    const cur = focus;
    const page = Math.max(1, perScreen - 1);
    switch (e.key) {
      case 'ArrowDown':
        e.preventDefault();
        moveFocus(cur == null ? firstIdx : cur + 1);
        break;
      case 'ArrowUp':
        e.preventDefault();
        moveFocus(cur == null ? firstIdx : cur - 1);
        break;
      case 'PageDown':
        e.preventDefault();
        moveFocus((cur ?? firstIdx) + page);
        break;
      case 'PageUp':
        e.preventDefault();
        moveFocus((cur ?? firstIdx) - page);
        break;
      case 'Home':
        e.preventDefault();
        moveFocus(0);
        break;
      case 'End':
        e.preventDefault();
        moveFocus(count - 1);
        break;
      case 'Enter': {
        if (cur == null) break;
        e.preventDefault();
        const row = rows[cur];
        if (row !== undefined) onActivate?.(row, cur);
        break;
      }
    }
  };

  // Roving tabindex: exactly one row is tabbable, and when nothing has been
  // focused yet it is the first *rendered* one — so Tab always lands somewhere
  // that exists, even when the list is scrolled 40,000 rows down.
  const tabIdx = focus ?? firstIdx;

  const slice: React.ReactNode[] = [];
  for (let i = startIdx; i < endIdx; i++) {
    const row = rows[i];
    if (row === undefined) continue;
    const key = rowKey(row, i);
    const extra = rowProps?.(row, i);
    const selected = selectedKey != null && selectedKey === key;
    slice.push(
      <div
        key={key}
        ref={(el) => {
          if (el) rowEls.current.set(i, el);
          else rowEls.current.delete(i);
        }}
        role="row"
        aria-rowindex={i + 2}
        aria-selected={selected || undefined}
        data-selected={selected ? 'true' : undefined}
        tabIndex={i === tabIdx ? 0 : -1}
        className={extra?.className ? `vrow ${extra.className}` : 'vrow'}
        style={{ transform: `translateY(${i * rowHeight}px)`, ...extra?.style }}
        onClick={() => {
          focusRef.current = i;
          setFocus(i);
          onActivate?.(row, i);
        }}
      >
        {renderRow(row, i)}
      </div>,
    );
  }

  return (
    <div
      ref={scrollRef}
      className={className ? `vtable ${className}` : 'vtable'}
      style={vars({ '--cols': cols, '--row-h': `${rowHeight}px` }, style)}
      role="grid"
      aria-label={label}
      // The full filtered count, not the window: a screen reader reading "row 3
      // of 40" while the user filtered 50,000 assets down to 12,000 would be
      // describing an implementation detail.
      aria-rowcount={count + 1}
      onScroll={onScroll}
      onKeyDown={onKeyDown}
    >
      <div className="vhead" role="row" aria-rowindex={1}>
        {columns.map((c) => {
          const sorted = sortKey === c.key;
          const ariaSort = sorted ? (sortDesc ? 'descending' : 'ascending') : undefined;
          if (!c.sortable || !onSort) {
            return (
              <div key={c.key} role="columnheader">
                {c.label}
              </div>
            );
          }
          return (
            <button
              key={c.key}
              type="button"
              role="columnheader"
              aria-sort={ariaSort}
              aria-label={`${c.label}, sort`}
              onClick={() => onSort(c.key)}
            >
              {c.label}
              <SortMark active={sorted} desc={sortDesc ?? false} />
            </button>
          );
        })}
      </div>

      {count === 0 ? (
        empty
      ) : (
        <div className="vtable-spacer" role="rowgroup" style={{ height: count * rowHeight }}>
          {slice}
        </div>
      )}
    </div>
  );
}

/** A caret rather than an arrow character: it inherits the header's colour
 *  (which globals.css turns accent when sorted) and holds its box when
 *  inactive, so switching the sort column never shifts the labels. */
function SortMark({ active, desc }: { active: boolean; desc: boolean }) {
  return (
    <svg
      width="9"
      height="9"
      viewBox="0 0 10 10"
      aria-hidden="true"
      style={{
        flex: 'none',
        opacity: active ? 1 : 0,
        transform: desc ? 'rotate(180deg)' : undefined,
        transition: 'opacity var(--dur-fast) var(--ease), transform var(--dur-fast) var(--ease)',
      }}
    >
      <path d="M5 1.5 8.5 7h-7z" fill="currentColor" />
    </svg>
  );
}
