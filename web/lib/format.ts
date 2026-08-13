/**
 * Locale-independent number formatting.
 *
 * `Number.prototype.toLocaleString()` with no arguments uses the *runtime's*
 * locale, which is not the same runtime in both places this app renders: the
 * export is prerendered by `next build` on a contributor's machine, then
 * hydrated in a visitor's browser. On a machine with an Arabic locale the
 * build emitted `٠` where the browser produced `0` — a hydration text
 * mismatch (React #418) on every page, which makes React discard the
 * server-rendered HTML and re-render the whole tree.
 *
 * It also broke reproducibility, which is the more expensive half. The export
 * is committed to internal/server/webui/ and CI rebuilds it and diffs to catch
 * a stale bundle; a build whose bytes depend on the builder's locale makes that
 * check fail for everyone whose locale differs from the last committer's.
 *
 * So: pin the locale. The UI is English throughout, so `en-US` grouping is the
 * consistent choice — Arabic-Indic digits inside an otherwise English table
 * were never intended either.
 */

const GROUPED = new Intl.NumberFormat('en-US');

/** Thousands-separated integer. Use everywhere a count is rendered. */
export function fmtCount(n: number): string {
  return GROUPED.format(n);
}

const ONE_DP = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 1,
  maximumFractionDigits: 1,
});

/** One decimal place, for durations in seconds and percentages. */
export function fmtDecimal(n: number): string {
  return ONE_DP.format(n);
}

/**
 * A duration in milliseconds, rendered at the coarsest unit that still says
 * something useful: sub-second work reads in ms, a long audit in minutes. A
 * run that took 4 minutes shown as "247.3s" makes the reader do the division.
 */
export function fmtDuration(ms: number): string {
  if (!Number.isFinite(ms) || ms < 0) return '—';
  if (ms < 1000) return `${Math.round(ms)}ms`;
  if (ms < 60_000) return `${fmtDecimal(ms / 1000)}s`;
  const mins = Math.floor(ms / 60_000);
  const secs = Math.round((ms % 60_000) / 1000);
  return `${mins}m ${String(secs).padStart(2, '0')}s`;
}

/** A share of a whole, as a percentage string. Sub-1% values keep a decimal
 *  so a long tail does not collapse into a column of "0%". */
export function fmtPercent(value: number, total: number): string {
  if (total <= 0) return '—';
  const pct = (value / total) * 100;
  if (pct > 0 && pct < 1) return `${fmtDecimal(pct)}%`;
  return `${Math.round(pct)}%`;
}
