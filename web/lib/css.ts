/**
 * React's `CSSProperties` is closed by design — no index signature — so a CSS
 * custom property set inline (`--rail`, `--meter-color`, `--seg-x`) does not
 * typecheck without an assertion.
 *
 * One helper with one comment, rather than the same three lines re-declared in
 * every component that paints a token-driven value: five copies had already
 * accumulated, and the next one would have been written from memory rather
 * than from this rationale.
 */
export function vars(
  custom: Record<string, string>,
  rest?: React.CSSProperties,
): React.CSSProperties {
  return { ...rest, ...custom } as unknown as React.CSSProperties;
}
