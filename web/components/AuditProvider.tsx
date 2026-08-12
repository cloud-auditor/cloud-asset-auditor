'use client';

import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from 'react';
import { fetchKubeContexts, fetchProviders, streamAudit } from '@/lib/api';
import { PROVIDERS_WITH_COLOR } from '@/lib/colors';
import type { ArrivalBucket, Asset, ProviderStat, Toast } from '@/lib/types';

/** What {@link AuditState.start} may override for a single run. */
export interface AuditScope {
  providers?: string[];
  kubeContexts?: string[];
}

/** A toast before AuditProvider stamps an id on it. */
export type ToastInput = Omit<Toast, 'id'>;

export interface AuditState {
  assets: Asset[];
  running: boolean;
  startedAt: string | null;
  elapsedMs: number | null;
  errors: string[];
  initErrors: string[];
  failure: string | null;

  /** Rolling per-second arrival counts, oldest first, at most 120 entries. */
  arrival: ArrivalBucket[];
  /** Per-provider progress. Keyed by provider name; '' holds unattributable errors. */
  byProvider: Map<string, ProviderStat>;

  providers: string[];
  selectedProviders: string[];
  setSelectedProviders: (p: string[]) => void;

  kubeContexts: string[];
  selectedKubeContexts: string[];
  setSelectedKubeContexts: (c: string[]) => void;

  toasts: Toast[];
  toast: (t: ToastInput) => void;
  dismissToast: (id: number) => void;

  start: (scope?: AuditScope) => void;
  stop: () => void;
  clear: () => void;
}

const Ctx = createContext<AuditState | null>(null);

const FLUSH_MS = 200;

/** ~2 minutes of history at one bucket per second — enough for a sparkline to
 *  show the shape of a run without growing without bound on a long audit. */
const MAX_BUCKETS = 120;

/** The queue is bounded here, not in the Toaster. The Toaster only mounts the
 *  newest few, and an unmounted toast never runs its dismiss timer — so an
 *  unbounded queue would strand the overflow and then resurface it the moment
 *  the visible ones faded. Keep this at or below the Toaster's MAX_VISIBLE. */
const MAX_TOASTS = 5;

const PROVIDERS_KEY = 'auditor-providers';
const KUBE_KEY = 'auditor-kube-contexts';

function isStringArray(v: unknown): v is string[] {
  return Array.isArray(v) && v.every((x) => typeof x === 'string');
}

/** Reads a persisted string list. Returns null for "nothing usable stored",
 *  which the caller must distinguish from an intentionally empty selection. */
function readList(key: string): string[] | null {
  try {
    const raw = window.localStorage.getItem(key);
    if (raw === null) return null;
    const parsed: unknown = JSON.parse(raw);
    return isStringArray(parsed) ? parsed : null;
  } catch {
    // Storage throws outright in private-mode Safari and under some cookie
    // policies, and JSON.parse throws on a hand-edited value. Neither is worth
    // failing the app over — the operator just re-picks their scope.
    return null;
  }
}

function writeList(key: string, v: string[]): void {
  try {
    window.localStorage.setItem(key, JSON.stringify(v));
  } catch {
    // Same as readList: the choice simply doesn't survive the reload.
  }
}

/**
 * Attributes an error line to the provider that raised it.
 *
 * There is no structural provider field to read: `handleAuditSSE` emits
 * `{"message": e.Error()}` verbatim (internal/server/handlers.go), and the
 * fan-in in internal/server/audit.go::forward passes errors through untouched.
 * What every provider *does* do is prefix its own name — `oci compartments: …`,
 * `cloudflare zones: …`, `kubernetes discovery (partial): …`, `gcp
 * searchAllResources (projects/x): …`, `netbird peers: …`, `tailscale devices:
 * …` — and the server's own init failures use `provider "oci" failed to
 * initialize: …` / `provider "x" is not registered`.
 *
 * So: read the quoted name if the server wrote one, else the first word if it
 * names a known provider. Anything else lands in the '' bucket rather than
 * being guessed at — mis-attributing an error is worse than not attributing it.
 */
function attributeError(msg: string, known: readonly string[]): string {
  const quoted = /^provider "([^"]+)"/.exec(msg);
  if (quoted) return quoted[1];
  const head = msg.split(/[\s:]/, 1)[0]?.toLowerCase() ?? '';
  return head !== '' && known.includes(head) ? head : '';
}

/** An error waiting for the next flush. `init` splits factory failures (which
 *  the UI renders distinctly) from collect-time ones without a second buffer. */
interface PendingError {
  msg: string;
  init: boolean;
}

/**
 * Holds the audit result for the whole app.
 *
 * State lives here — above the router — so navigating Dashboard → Assets →
 * Topology does not discard a completed audit. Re-running providers on every
 * tab switch would burn the operator's API quota and, on a large tenancy,
 * take minutes each time.
 */
export function AuditProvider({ children }: { children: React.ReactNode }) {
  const [assets, setAssets] = useState<Asset[]>([]);
  const [running, setRunning] = useState(false);
  const [startedAt, setStartedAt] = useState<string | null>(null);
  const [elapsedMs, setElapsedMs] = useState<number | null>(null);
  const [errors, setErrors] = useState<string[]>([]);
  const [initErrors, setInitErrors] = useState<string[]>([]);
  const [failure, setFailure] = useState<string | null>(null);
  const [arrival, setArrival] = useState<ArrivalBucket[]>([]);
  const [byProvider, setByProvider] = useState<Map<string, ProviderStat>>(() => new Map());
  const [toasts, setToasts] = useState<Toast[]>([]);

  const [providers, setProviders] = useState<string[]>([]);
  const [selectedProviders, setSelectedProviders] = useState<string[]>([]);
  const [kubeContexts, setKubeContexts] = useState<string[]>([]);
  const [selectedKubeContexts, setSelectedKubeContexts] = useState<string[]>([]);
  // Until the stored scope has been read, a write would persist the empty
  // initial state over it — so every write waits for this.
  const [hydrated, setHydrated] = useState(false);

  const abortRef = useRef<(() => void) | null>(null);

  // A high-cardinality audit emits tens of thousands of `asset` events. One
  // setState per event would re-render the table tens of thousands of times
  // and lock the tab up, so events are batched into a buffer and flushed on a
  // timer — the stream stays live-looking without the render storm.
  const buffer = useRef<Asset[]>([]);
  const errBuffer = useRef<PendingError[]>([]);
  const flushTimer = useRef<ReturnType<typeof setInterval> | null>(null);

  // Derived stream state lives in refs as well as state: the flush runs off a
  // timer, so it must be able to read the previous value without the closure
  // that created it going stale.
  const statsRef = useRef<Map<string, ProviderStat>>(new Map());
  const arrivalRef = useRef<ArrivalBucket[]>([]);
  const runningRef = useRef(false);
  const knownRef = useRef<readonly string[]>(PROVIDERS_WITH_COLOR);

  const toastSeq = useRef(0);

  /** Ids come from a counter, not Date.now() or Math.random(): two toasts
   *  raised in the same millisecond must not collide into one React key. */
  const toast = useCallback((t: ToastInput) => {
    toastSeq.current += 1;
    const next: Toast = { ...t, id: toastSeq.current };
    setToasts((prev) => prev.concat(next).slice(-MAX_TOASTS));
  }, []);

  const dismissToast = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  // Init errors all arrive in one synchronous burst before the first asset
  // (see handleAuditSSE), so a short debounce collects the whole set and
  // summarises it into one toast instead of one per skipped provider.
  const pendingInit = useRef<string[]>([]);
  const initToastTimer = useRef<ReturnType<typeof setTimeout> | null>(null);

  const flushInitToast = useCallback(() => {
    initToastTimer.current = null;
    const msgs = pendingInit.current;
    pendingInit.current = [];
    if (msgs.length === 0) return;
    if (msgs.length === 1) {
      toast({ kind: 'warn', title: 'Provider skipped', body: msgs[0] });
      return;
    }
    const names = msgs.map((m) => attributeError(m, knownRef.current)).filter(Boolean);
    toast({
      kind: 'warn',
      title: `${msgs.length} providers skipped`,
      body: names.length === msgs.length ? names.join(', ') : msgs.join(' · '),
    });
  }, [toast]);

  /**
   * Drains the buffers into state. Called on a timer while a run is live and
   * once more on stop, so the tail of the stream is never left in the buffer.
   */
  const flush = useCallback(() => {
    const batch = buffer.current;
    const errBatch = errBuffer.current;
    const now = Date.now();
    const second = Math.floor(now / 1000) * 1000;

    // Buckets advance while the run is live even with nothing to record: an
    // idle provider should draw as a flat second, not as a hole the sparkline
    // would connect straight through.
    if (runningRef.current || batch.length > 0) {
      const prev = arrivalRef.current;
      const last = prev.length > 0 ? prev[prev.length - 1] : undefined;
      let next = prev;
      if (!last) {
        next = [{ t: second, n: batch.length }];
      } else if (last.t === second) {
        if (batch.length > 0) {
          next = prev.slice(0, -1).concat({ t: second, n: last.n + batch.length });
        }
      } else {
        // Only the last MAX_BUCKETS seconds can survive the trim below, so a
        // backgrounded tab (or a sleeping laptop) can't make this loop long.
        next = prev.slice();
        const from = Math.max(last.t + 1000, second - MAX_BUCKETS * 1000);
        for (let t = from; t < second; t += 1000) next.push({ t, n: 0 });
        next.push({ t: second, n: batch.length });
      }
      if (next.length > MAX_BUCKETS) next = next.slice(next.length - MAX_BUCKETS);
      if (next !== prev) {
        arrivalRef.current = next;
        setArrival(next);
      }
    }

    if (batch.length > 0 || errBatch.length > 0) {
      const stats = new Map(statsRef.current);
      for (const a of batch) {
        const cur = stats.get(a.provider);
        stats.set(
          a.provider,
          cur
            ? { ...cur, count: cur.count + 1, lastAt: now }
            : { count: 1, firstAt: now, lastAt: now, errors: 0 },
        );
      }
      for (const e of errBatch) {
        const name = attributeError(e.msg, knownRef.current);
        const cur = stats.get(name);
        stats.set(
          name,
          cur
            ? { ...cur, errors: cur.errors + 1 }
            : { count: 0, firstAt: now, lastAt: now, errors: 1 },
        );
      }
      statsRef.current = stats;
      setByProvider(stats);
    }

    if (batch.length > 0) {
      buffer.current = [];
      setAssets((prev) => prev.concat(batch));
    }
    if (errBatch.length > 0) {
      errBuffer.current = [];
      const collectErrs = errBatch.filter((e) => !e.init).map((e) => e.msg);
      const initErrs = errBatch.filter((e) => e.init).map((e) => e.msg);
      if (collectErrs.length > 0) setErrors((prev) => prev.concat(collectErrs));
      if (initErrs.length > 0) setInitErrors((prev) => prev.concat(initErrs));
    }
  }, []);

  useEffect(() => {
    const ctl = new AbortController();
    fetchProviders(ctl.signal)
      .then((r) => {
        // Union rather than replace: the registry is the truth for what can be
        // run, but a provider absent from it can still appear in an error
        // prefix (a stale selection, or `--demo` on a server we polled before).
        knownRef.current = Array.from(new Set([...PROVIDERS_WITH_COLOR, ...r.providers]));
        setProviders(r.providers);
      })
      .catch(() => {});
    fetchKubeContexts(ctl.signal)
      .then((r) => setKubeContexts(r.contexts))
      .catch(() => {});
    return () => ctl.abort();
  }, []);

  // Restore the operator's scope. In an effect, never during render: this app
  // is prerendered by `next build`, where there is no window, and a value read
  // during render would also disagree with the server-rendered markup.
  useEffect(() => {
    const p = readList(PROVIDERS_KEY);
    if (p) setSelectedProviders(p);
    const k = readList(KUBE_KEY);
    if (k) setSelectedKubeContexts(k);
    setHydrated(true);
  }, []);

  useEffect(() => {
    if (hydrated) writeList(PROVIDERS_KEY, selectedProviders);
  }, [hydrated, selectedProviders]);

  useEffect(() => {
    if (hydrated) writeList(KUBE_KEY, selectedKubeContexts);
  }, [hydrated, selectedKubeContexts]);

  // Drop restored names the server no longer offers — a provider that was
  // unregistered (or a kube context removed from the operator's kubeconfig)
  // would otherwise be sent on every run and come back as an init error. The
  // identity check keeps this from looping: an unchanged list is returned as-is.
  useEffect(() => {
    if (!hydrated || providers.length === 0) return;
    setSelectedProviders((prev) => {
      const keep = prev.filter((p) => providers.includes(p));
      return keep.length === prev.length ? prev : keep;
    });
  }, [hydrated, providers]);

  useEffect(() => {
    if (!hydrated || kubeContexts.length === 0) return;
    setSelectedKubeContexts((prev) => {
      const keep = prev.filter((c) => kubeContexts.includes(c));
      return keep.length === prev.length ? prev : keep;
    });
  }, [hydrated, kubeContexts]);

  const stop = useCallback(() => {
    abortRef.current?.();
    abortRef.current = null;
    if (flushTimer.current) {
      clearInterval(flushTimer.current);
      flushTimer.current = null;
    }
    runningRef.current = false;
    flush();
    setRunning(false);
  }, [flush]);

  const clear = useCallback(() => {
    stop();
    buffer.current = [];
    errBuffer.current = [];
    statsRef.current = new Map();
    arrivalRef.current = [];
    setAssets([]);
    setErrors([]);
    setInitErrors([]);
    setFailure(null);
    setStartedAt(null);
    setElapsedMs(null);
    setArrival([]);
    setByProvider(new Map());
  }, [stop]);

  const start = useCallback(
    (scope?: AuditScope) => {
      // Reset in place rather than through clear(): clear() also drops
      // startedAt, and the previous run's start time is what the header shows
      // until the new run's `meta` event lands a few hundred ms later.
      abortRef.current?.();
      buffer.current = [];
      errBuffer.current = [];
      statsRef.current = new Map();
      arrivalRef.current = [];
      pendingInit.current = [];
      setAssets([]);
      setErrors([]);
      setInitErrors([]);
      setFailure(null);
      setElapsedMs(null);
      setArrival([]);
      setByProvider(new Map());
      runningRef.current = true;
      setRunning(true);

      // An explicit scope (the dashboard's "load demo data", the command
      // palette) also becomes the visible selection — a run whose scope the
      // picker disagrees with is a lie about what is on screen.
      const runProviders = scope?.providers ?? selectedProviders;
      const runContexts = scope?.kubeContexts ?? selectedKubeContexts;
      if (scope?.providers) setSelectedProviders(scope.providers);
      if (scope?.kubeContexts) setSelectedKubeContexts(scope.kubeContexts);

      flushTimer.current = setInterval(flush, FLUSH_MS);

      abortRef.current = streamAudit(
        { providers: runProviders, kubeContexts: runContexts },
        {
          onMeta: (m) => setStartedAt(m.started_at),
          onAsset: (a) => buffer.current.push(a),
          onInitError: (m) => {
            errBuffer.current.push({ msg: m, init: true });
            pendingInit.current.push(m);
            if (initToastTimer.current) clearTimeout(initToastTimer.current);
            initToastTimer.current = setTimeout(flushInitToast, 400);
          },
          onError: (m) => errBuffer.current.push({ msg: m, init: false }),
          onDone: (d) => {
            setElapsedMs(d.elapsed_ms);
            stop();
            toast({
              kind: d.errors > 0 ? 'warn' : 'ok',
              title: 'Audit finished',
              body:
                `${d.count.toLocaleString()} assets in ${(d.elapsed_ms / 1000).toFixed(1)}s` +
                (d.errors > 0 ? ` · ${d.errors} error${d.errors === 1 ? '' : 's'}` : ''),
            });
          },
          onFailure: (err) => {
            setFailure(err.message);
            stop();
            toast({ kind: 'err', title: 'Audit stream failed', body: err.message });
          },
        },
      );
    },
    [flush, flushInitToast, selectedProviders, selectedKubeContexts, stop, toast],
  );

  // `?run=1` starts an audit on load, optionally scoped by `?providers=` and
  // `?kube_contexts=`. It makes the UI drivable without a click, which is what
  // a screenshot pipeline, a kiosk dashboard, and a bookmarked "my scope, now"
  // link all need.
  //
  // It waits for `hydrated` so the restored selection is in place before the
  // run, and fires at most once per page load — `autoRan` is a ref rather than
  // state because re-running an audit is expensive and a second trigger from a
  // re-render would be silent.
  const autoRan = useRef(false);
  useEffect(() => {
    if (!hydrated || autoRan.current) return;
    const q = new URLSearchParams(window.location.search);
    if (q.get('run') !== '1' && q.get('run') !== 'true') return;
    autoRan.current = true;
    const list = (k: string) =>
      q.get(k)?.split(',').map((s) => s.trim()).filter(Boolean) ?? undefined;
    start({ providers: list('providers'), kubeContexts: list('kube_contexts') });
    // `start` is intentionally omitted: it is re-created whenever the selection
    // changes, and depending on it would re-arm this effect mid-run.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [hydrated]);

  // Abort any in-flight audit if the app unmounts, so a closed tab doesn't
  // leave the server collecting into a dead connection.
  useEffect(
    () => () => {
      abortRef.current?.();
      if (initToastTimer.current) clearTimeout(initToastTimer.current);
    },
    [],
  );

  const value = useMemo<AuditState>(
    () => ({
      assets,
      running,
      startedAt,
      elapsedMs,
      errors,
      initErrors,
      failure,
      arrival,
      byProvider,
      providers,
      selectedProviders,
      setSelectedProviders,
      kubeContexts,
      selectedKubeContexts,
      setSelectedKubeContexts,
      toasts,
      toast,
      dismissToast,
      start,
      stop,
      clear,
    }),
    [
      assets,
      running,
      startedAt,
      elapsedMs,
      errors,
      initErrors,
      failure,
      arrival,
      byProvider,
      providers,
      selectedProviders,
      setSelectedProviders,
      kubeContexts,
      selectedKubeContexts,
      setSelectedKubeContexts,
      toasts,
      toast,
      dismissToast,
      start,
      stop,
      clear,
    ],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useAudit(): AuditState {
  const v = useContext(Ctx);
  if (!v) throw new Error('useAudit must be used inside <AuditProvider>');
  return v;
}
