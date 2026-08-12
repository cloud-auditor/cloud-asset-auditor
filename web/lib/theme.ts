'use client';

import { useCallback, useEffect, useState } from 'react';

export type Theme = 'light' | 'dark' | 'system';

const KEY = 'auditor-theme';

/** Broadcast channel for same-document hook instances. Two components can call
 *  useTheme() (the topbar toggle and the command palette both do); without a
 *  notification they would each hold a private copy of the state and disagree
 *  the moment one of them writes. */
const EVENT = 'auditor-theme-change';

/**
 * Runs in <head> before first paint, so the page never renders light and
 * repaints dark. Kept as a hand-minified string because it is inlined into the
 * HTML rather than bundled — there is no build step between here and the
 * document, so what is written is what ships.
 *
 * Only an explicit choice is written to the attribute: absent means "system",
 * which globals.css handles with a prefers-color-scheme media query.
 *
 * `?theme=light|dark` wins over the stored value and is then persisted, so a
 * link can pin the appearance it was written for. That matters for anything
 * that drives the page without a pointer — a kiosk display, a screenshot run
 * — where there is no way to click the toggle, and it is the same reason
 * `?run=1` exists (see AuditProvider).
 */
export const THEME_INIT_SCRIPT =
  `(function(){try{var k=${JSON.stringify(KEY)},` +
  `q=new URLSearchParams(location.search).get("theme"),` +
  `t=(q==="light"||q==="dark")?q:localStorage.getItem(k);` +
  `if(q==="light"||q==="dark")localStorage.setItem(k,q);` +
  `if(t==="light"||t==="dark")document.documentElement.dataset.theme=t;}catch(e){}})();`;

function isTheme(v: unknown): v is Theme {
  return v === 'light' || v === 'dark' || v === 'system';
}

function apply(t: Theme): void {
  // removeAttribute rather than `delete dataset.theme`: same effect, but it
  // does not depend on how strictly the DOMStringMap index signature is typed.
  if (t === 'system') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.dataset.theme = t;
}

export function useTheme(): { theme: Theme; setTheme: (t: Theme) => void; resolved: 'light' | 'dark' } {
  // Both of these start at a constant. The static export prerenders this
  // component at build time, where `window` does not exist, and a value read
  // during render would also desync from the markup at hydration. The real
  // values land in the effect below; the *page* is already correct by then
  // because THEME_INIT_SCRIPT set the attribute before React ran.
  const [theme, setThemeState] = useState<Theme>('system');
  const [systemDark, setSystemDark] = useState(true);

  useEffect(() => {
    const mq = window.matchMedia('(prefers-color-scheme: dark)');
    setSystemDark(mq.matches);

    const read = () => {
      let stored: string | null = null;
      try {
        stored = window.localStorage.getItem(KEY);
      } catch {
        // Storage can throw outright in private-mode Safari and under a
        // restrictive cookie policy. A themeless session is fine; a crashed
        // topbar is not.
      }
      const next = isTheme(stored) ? stored : 'system';
      // Also reassert the attribute: this same reader handles the cross-tab
      // `storage` event, where nothing else has touched this document's <html>.
      apply(next);
      setThemeState(next);
    };
    read();

    const onScheme = (e: MediaQueryListEvent) => setSystemDark(e.matches);
    const onStorage = (e: StorageEvent) => {
      if (e.key === null || e.key === KEY) read();
    };
    mq.addEventListener('change', onScheme);
    window.addEventListener('storage', onStorage);
    window.addEventListener(EVENT, read);
    return () => {
      mq.removeEventListener('change', onScheme);
      window.removeEventListener('storage', onStorage);
      window.removeEventListener(EVENT, read);
    };
  }, []);

  const setTheme = useCallback((t: Theme) => {
    apply(t);
    setThemeState(t);
    try {
      window.localStorage.setItem(KEY, t);
    } catch {
      // Same as above — the choice just does not survive a reload.
    }
    window.dispatchEvent(new Event(EVENT));
  }, []);

  return { theme, setTheme, resolved: theme === 'system' ? (systemDark ? 'dark' : 'light') : theme };
}
