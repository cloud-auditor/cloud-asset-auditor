/**
 * Subsequence matcher for the command palette.
 *
 * Scoring rationale: for a list of short labels ("Run audit", "Export CSV",
 * an asset id), *where* a character matched says far more than how many
 * matched. So a hit earns a small base point and then bonuses for the signals
 * a human actually reads — the very start of the string, the start of a word,
 * and a run of adjacent characters — while a long skip between hits costs a
 * bounded amount. The final length penalty is the tiebreak: given two labels
 * that match equally well, the shorter one is the more specific answer.
 */

export interface FuzzyMatch {
  score: number;
  /** Half-open [start, end) spans of the haystack that matched. */
  ranges: [number, number][];
}

const BOUNDARY = /[\s\-_./:@]/;

/** Returns null when `needle` is not a subsequence of `haystack`. */
export function match(needle: string, haystack: string): FuzzyMatch | null {
  const n = needle.trim().toLowerCase();
  if (!n) return { score: 0, ranges: [] };
  const h = haystack.toLowerCase();
  if (n.length > h.length) return null;

  const ranges: [number, number][] = [];
  let score = 0;
  let from = 0;
  let lastAt = -2;
  let run = 0;

  for (let i = 0; i < n.length; i++) {
    const c = n[i];
    // A space in the query is a separator between words the user typed, not a
    // character to find — "run aud" should still match "Run audit".
    if (c === ' ') {
      lastAt = -2;
      run = 0;
      continue;
    }
    const at = h.indexOf(c, from);
    if (at === -1) return null;

    let s = 1;
    if (at === 0) s += 12;
    else if (BOUNDARY.test(haystack[at - 1])) s += 8;
    else if (haystack[at - 1] === haystack[at - 1].toLowerCase() && haystack[at] !== h[at]) {
      s += 6; // camelCase hump reads as a word start too
    }

    if (at === lastAt + 1) {
      run++;
      s += 4 + Math.min(run, 4);
    } else {
      run = 0;
      if (i > 0) s -= Math.min(at - from, 6) * 0.5;
    }

    score += s;
    const tail = ranges[ranges.length - 1];
    if (tail && tail[1] === at) tail[1] = at + 1;
    else ranges.push([at, at + 1]);
    lastAt = at;
    from = at + 1;
  }

  return { score: score - h.length * 0.05, ranges };
}
