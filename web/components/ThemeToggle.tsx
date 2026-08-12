'use client';

import { useTheme, type Theme } from '@/lib/theme';

/**
 * Three explicit segments rather than one cycling button.
 *
 * A cycling control has to answer "what is set now?" and "what happens if I
 * press?" with the same 30px of glyph, and it cannot express the third state
 * at all — "system" looks identical to whichever theme the OS resolved it to.
 * Three segments cost ~80px of a 56px bar that has room for them, state the
 * current mode outright, and reach any mode in one press.
 *
 * No `.thumb` here: with three fixed-width buttons the sliding indicator would
 * travel 26px, which reads as a flicker rather than motion. The control falls
 * back to `.segmented button[data-on='true']` painting its own pill, which
 * globals.css styles for exactly this case.
 */
const MODES: { value: Theme; label: string; icon: () => React.ReactElement }[] = [
  { value: 'light', label: 'Light theme', icon: Sun },
  { value: 'system', label: 'Match system theme', icon: Monitor },
  { value: 'dark', label: 'Dark theme', icon: Moon },
];

export function ThemeToggle() {
  const { theme, setTheme } = useTheme();

  return (
    <div className="segmented" role="group" aria-label="Colour theme">
      {MODES.map((m) => {
        const on = theme === m.value;
        const Icon = m.icon;
        return (
          <button
            key={m.value}
            type="button"
            className="btn icon sm"
            data-on={on ? 'true' : 'false'}
            aria-label={m.label}
            aria-pressed={on}
            title={m.label}
            onClick={() => setTheme(m.value)}
          >
            <Icon />
          </button>
        );
      })}
    </div>
  );
}

/* Glyphs are 14px on a 24px button and stroke with currentColor, so the
   segmented control's own active/inactive colours drive them. */

function Sun() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <circle cx="12" cy="12" r="4.2" stroke="currentColor" strokeWidth="1.8" />
      <path
        d="M12 2.6v2.2M12 19.2v2.2M2.6 12h2.2M19.2 12h2.2M5.4 5.4l1.6 1.6M17 17l1.6 1.6M18.6 5.4L17 7M7 17l-1.6 1.6"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinecap="round"
      />
    </svg>
  );
}

function Monitor() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <rect x="2.8" y="4" width="18.4" height="12.4" rx="2" stroke="currentColor" strokeWidth="1.8" />
      <path d="M8.5 20h7M12 16.4V20" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" />
    </svg>
  );
}

function Moon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" aria-hidden="true">
      <path
        d="M20.2 14.4A8.6 8.6 0 0 1 9.6 3.8a8.6 8.6 0 1 0 10.6 10.6Z"
        stroke="currentColor"
        strokeWidth="1.8"
        strokeLinejoin="round"
      />
    </svg>
  );
}
