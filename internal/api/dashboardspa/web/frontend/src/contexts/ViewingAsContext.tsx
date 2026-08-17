import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from 'react';
import { errorMessage, type ViewingAs } from 'gas-city-dashboard-shared';
import { prioritizeAliases, type AliasBucket } from '../hooks/aliasPriority';
import {
  readBrowserStorage,
  removeBrowserStorage,
  writeBrowserStorage,
} from '../lib/browserStorage';
import { reportClientError } from '../lib/clientErrorReporting';
import { listSupervisorMail } from '../supervisor/mailReads';
import { listSupervisorSessions } from '../supervisor/sessionReads';
import { useOperatorConfig } from './OperatorConfigContext';

// Identity-switching for mail:
//
//   Frontend: visible "Reading as <agent>" strip with accent color when
//   ≠ the operator. The compose-from field is disabled while
//   impersonating. THE CONSTRAINT IS VISIBLE.
//
//   No client-side caching of mail under as-identity: Cache-Control:
//   no-store + no localStorage retention.
//
// We use sessionStorage so the chosen identity survives accidental page
// refresh in the same tab but does NOT persist beyond tab close — the
// "no retention" rule applies to cached mail bodies, not the user's
// chosen viewing context, but tab-scoped is friendlier here than fully
// transient.
//
// The provider also owns the alias-list prefetch. Consumers call
// loadAliases() — first call triggers the fetch, subsequent calls are
// no-ops. Lazy so non-Mail routes don't pay the cost
// (gascity-dashboard-e85 code-reviewer HIGH-1). Failures fall back to a
// list containing just the operator — the UI degrades but never breaks.

const STORAGE_KEY = 'gascity.dashboard.viewingAs';
const COMPONENT = 'ViewingAsContext';
const ALIAS_RE = /^[a-z][a-z0-9_./-]{1,63}$/i;

// Bounded retry schedule for supervisor sessions (gascity-dashboard-5gg).
// A transient 504 on first page load used to latch sessionsUnavailable
// permanently because loadAliases() is one-shot. Schedule three retries
// with growing backoff so a recovering supervisor flips the footnote
// off; after the third failure the flag stays sticky (matching the old
// terminal behaviour).
//
// `as const` pins the literal tuple shape so the array can't be silently
// mutated or re-assigned to a different length elsewhere. Combined with
// the `getSessionsRetryDelay` runtime guard below, an out-of-bounds index
// can never reach `setTimeout` as `undefined` — which would otherwise fire
// at 0 ms and produce a busy-loop retry storm (gascity-dashboard-7ky).
const SESSIONS_RETRY_DELAYS_MS = [30_000, 90_000, 270_000] as const;

// Safe-by-construction accessor for the retry schedule. Returns the delay
// for the given attempt index, or `null` when the index is out of bounds
// or otherwise invalid (negative, non-integer, NaN). The caller MUST treat
// `null` as "stop retrying" — never substitute a fallback delay and never
// coerce it to a number. Co-locating the bounds check with the indexed
// read closes the noUncheckedIndexedAccess gap: even if a future refactor
// weakens the caller's own bounds check, this accessor cannot return
// `undefined`.
//
// We deliberately do NOT log here. Frontend production code has
// `no-console: error`, and the silent-`null` return is the contract — the
// caller decides whether and how to report. The bounds-failed branch
// returns null indistinguishably from the "exhausted the schedule" branch
// so callers can treat both uniformly.
export function getSessionsRetryDelay(attemptIndex: number): number | null {
  if (!Number.isInteger(attemptIndex) || attemptIndex < 0) {
    return null;
  }
  if (attemptIndex >= SESSIONS_RETRY_DELAYS_MS.length) {
    return null;
  }
  const delay = SESSIONS_RETRY_DELAYS_MS[attemptIndex];
  // Defense-in-depth: the bounds check above logically excludes this
  // branch, but `noUncheckedIndexedAccess` types the read as
  // `number | undefined`, so the explicit guard is required for the
  // accessor to honor its `number | null` return type without a non-null
  // assertion. Silent `null` keeps the no-log contract.
  if (delay === undefined) return null;
  return delay;
}

interface ViewingAsContextValue {
  viewingAs: ViewingAs;
  setAlias: (alias: string) => void;
  resetToOperator: () => void;
  /** Prioritized alias buckets (you / mayor / active / other). */
  aliasBuckets: ReadonlyArray<AliasBucket>;
  /** True iff loadAliases() has been called and the fetch is in-flight. */
  aliasesLoading: boolean;
  /**
   * True iff loadAliases() ran and the supervisor sessions fetch failed
   * (timeout or other upstream error). The mail-derived alias list is still
   * populated; consumers use this flag to swap a generic "loading more"
   * footnote for a terminal "agent list unavailable" message rather than
   * stay ambiguous. Mail panel uses this for the gascity-dashboard-xba
   * degraded-state copy.
   */
  sessionsUnavailable: boolean;
  /**
   * Idempotently trigger the alias prefetch. First caller starts the
   * fetch; subsequent callers are no-ops. Routes that need the dropdown
   * (Mail) call this on mount; other routes never pay the cost.
   */
  loadAliases: () => void;
}

const Context = createContext<ViewingAsContextValue | null>(null);

function readStored(operator: string): string {
  const stored = readBrowserStorage('sessionStorage', STORAGE_KEY, COMPONENT);
  if (stored.status === 'found') {
    const raw = stored.value;
    if (raw.length > 0 && raw.length <= 64) return raw;
  }
  return operator;
}

function writeStored(alias: string, operator: string): void {
  if (alias === operator) {
    removeBrowserStorage('sessionStorage', STORAGE_KEY, COMPONENT);
  } else {
    writeBrowserStorage('sessionStorage', STORAGE_KEY, alias, COMPONENT);
  }
}

export function ViewingAsProvider({ children }: { children: ReactNode }) {
  // Operator identity from /config (gascity-dashboard-bhvn). OPERATOR is the
  // display identity; the full config (incl. the gc mail-wire alias) is threaded
  // into the mail read below, which maps the operator's display alias to the
  // wire alias the supervisor addresses mail to/from.
  const operatorConfig = useOperatorConfig();
  const { operatorAlias: OPERATOR } = operatorConfig;
  const [alias, setAliasState] = useState<string>(() => readStored(OPERATOR));
  // OPERATOR is the neutral fallback ('operator') until /config resolves, then
  // flips to the real operator alias. The lazy `alias` initializer ran with the
  // fallback, so re-sync it when OPERATOR transitions — but only if the user
  // hasn't picked a different identity in the meantime (gascity-dashboard-bhvn).
  // Without this, isOperator (alias === OPERATOR) would latch false after config
  // lands, mis-showing the "reading as" banner and gating compose controls.
  const prevOperatorRef = useRef<string>(OPERATOR);
  const [sessionAliases, setSessionAliases] = useState<string[]>([]);
  const [mailFromOrTo, setMailFromOrTo] = useState<string[]>([]);
  const [aliasesLoading, setAliasesLoading] = useState<boolean>(false);
  const [sessionsUnavailable, setSessionsUnavailable] = useState<boolean>(false);
  // Single-flight guard: first loadAliases call wins; subsequent calls
  // (re-renders, StrictMode double-mount, multiple consumers) are no-ops.
  const startedRef = useRef<boolean>(false);
  // The mounted flag protects state setters from firing after provider
  // unmount. Initialized via a ref-callback in the effect below so that
  // StrictMode's mount → cleanup → re-mount cycle correctly leaves the
  // ref `true` between the two mounts. (A naive `useRef(true)` + cleanup
  // pattern leaves the ref permanently `false` after the first cleanup.)
  const mountedRef = useRef<boolean>(true);
  // Pending sessions-retry timer. Cleared on unmount, and replaced on
  // each scheduled retry so we never have more than one in flight.
  const sessionsRetryTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const setAlias = useCallback(
    (next: string) => {
      setAliasState(next);
      writeStored(next, OPERATOR);
    },
    [OPERATOR],
  );

  const resetToOperator = useCallback(() => {
    setAliasState(OPERATOR);
    writeStored(OPERATOR, OPERATOR);
  }, [OPERATOR]);

  // One attempt at supervisor sessions. Returns a promise that resolves
  // to `true` on success (state updated) or `false` on failure. Extracted
  // so the retry loop can call it without duplicating the parsing.
  const attemptSessionsFetch = useCallback(async (): Promise<boolean> => {
    try {
      const sessions = await listSupervisorSessions();
      if (!mountedRef.current) return true; // unmounted; bail without state work
      const seen = new Set<string>();
      const out: string[] = [];
      for (const s of sessions.items ?? []) {
        if (typeof s.alias !== 'string') continue;
        if (!ALIAS_RE.test(s.alias)) continue;
        const key = s.alias.toLowerCase();
        if (seen.has(key)) continue;
        seen.add(key);
        out.push(s.alias);
      }
      setSessionAliases(out);
      // Recovery: if a prior attempt had latched the degraded flag, flip
      // it back so the Mail footnote disappears (gascity-dashboard-5gg).
      setSessionsUnavailable(false);
      return true;
    } catch (err) {
      void reportClientError({
        component: COMPONENT,
        operation: 'loadAliases.sessions',
        message: errorMessage(err),
      });
      return false;
    }
  }, []);

  // Schedule the next retry from the SESSIONS_RETRY_DELAYS_MS table.
  // `attemptIndex` is the 0-based index of the NEXT retry attempt
  // (i.e. after `attemptIndex` failed retries have already happened, or
  // `attemptIndex === 0` for the first retry after the initial failure).
  // `getSessionsRetryDelay` returns `null` once the schedule is
  // exhausted, after which the flag stays sticky.
  const scheduleSessionsRetry = useCallback(
    (attemptIndex: number) => {
      if (!mountedRef.current) return;
      const delay = getSessionsRetryDelay(attemptIndex);
      if (delay === null) return;
      sessionsRetryTimerRef.current = setTimeout(() => {
        sessionsRetryTimerRef.current = null;
        if (!mountedRef.current) return;
        void attemptSessionsFetch()
          .then((ok) => {
            if (!mountedRef.current) return;
            if (!ok) scheduleSessionsRetry(attemptIndex + 1);
          })
          .catch((err) => {
            void reportClientError({
              component: COMPONENT,
              operation: 'loadAliases.sessionsRetry',
              message: errorMessage(err),
            });
          });
      }, delay);
    },
    [attemptSessionsFetch],
  );

  // Idempotent lazy prefetch. First caller wins; subsequent calls
  // (re-renders, multiple consumers) are no-ops. Both fetches are read
  // requests; either failing leaves the dropdown with just the operator
  // entry (still functional). Sessions failure triggers a bounded retry
  // schedule (gascity-dashboard-5gg); mail failure does not retry —
  // its blast radius is smaller and the corpus call is fast.
  const loadAliases = useCallback(() => {
    if (startedRef.current) return;
    startedRef.current = true;
    setAliasesLoading(true);

    // Resolve each source independently rather than awaiting both: the
    // mail corpus returns in ~0.5s while supervisor sessions can take many
    // seconds (or time out). Gating both behind Promise.allSettled held
    // the fast mail-derived aliases hostage to the slow one, leaving the
    // agent panel stuck on "loading" for the full sessions latency.
    // Loading clears only when BOTH have settled (footnote, not full
    // block), but the lists populate the moment each arrives.
    let pending = 2;
    const settleOne = () => {
      pending -= 1;
      if (pending === 0 && mountedRef.current) setAliasesLoading(false);
    };

    void attemptSessionsFetch()
      .then((ok) => {
        if (!mountedRef.current) return;
        if (!ok) {
          // sessions unavailable — panel still works off mail-derived
          // aliases, but flag it so the Mail agent panel can swap its
          // "Loading more agents" footnote for a terminal "agent list
          // unavailable" message (gascity-dashboard-xba). Schedule the
          // first retry; subsequent retries chain off each other.
          setSessionsUnavailable(true);
          scheduleSessionsRetry(0);
        }
      })
      .finally(settleOne);

    void listSupervisorMail('all', OPERATOR, operatorConfig)
      .then((mail) => {
        if (!mountedRef.current) return;
        const seen = new Set<string>();
        const out: string[] = [];
        for (const m of mail.items) {
          for (const candidate of [m.from, m.to]) {
            if (typeof candidate !== 'string' || candidate.length === 0) continue;
            if (!ALIAS_RE.test(candidate)) continue;
            const key = candidate.toLowerCase();
            if (seen.has(key)) continue;
            seen.add(key);
            out.push(candidate);
          }
        }
        setMailFromOrTo(out);
      })
      .catch((err) => {
        void reportClientError({
          component: COMPONENT,
          operation: 'loadAliases.mail',
          message: errorMessage(err),
        });
      })
      .finally(settleOne);
  }, [attemptSessionsFetch, scheduleSessionsRetry, OPERATOR, operatorConfig]);

  // Re-mount-aware mounted flag. The effect body sets mountedRef.current
  // = true on every mount (covering StrictMode's mount → cleanup → re-mount
  // cycle), and the cleanup sets it false on real unmount. The async IIFE
  // checks this flag before touching state. Cleanup also clears any
  // pending sessions-retry timer so a late firing can't trigger a
  // post-unmount supervisor sessions call (gascity-dashboard-5gg).
  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      if (sessionsRetryTimerRef.current !== null) {
        clearTimeout(sessionsRetryTimerRef.current);
        sessionsRetryTimerRef.current = null;
      }
    };
  }, []);

  // Re-sync `alias` to the operator identity when OPERATOR resolves from the
  // pre-config fallback to the real value. Guarded on `alias === prev` so a
  // user who already switched identity in the fallback window is not yanked
  // back; readStored() still honours a stored impersonation alias.
  useEffect(() => {
    const prev = prevOperatorRef.current;
    prevOperatorRef.current = OPERATOR;
    if (prev !== OPERATOR && alias === prev) {
      setAliasState(readStored(OPERATOR));
    }
  }, [OPERATOR, alias]);

  const aliasBuckets = useMemo(
    () =>
      prioritizeAliases({
        operator: OPERATOR,
        // Inject the current alias so the <select> always has a matching
        // <option> even if a stored alias has aged out of supervisor sessions
        // (code-reviewer HIGH-2: prevents blank-selection visual mismatch).
        sessionAliases: sessionAliases.includes(alias)
          ? sessionAliases
          : [...sessionAliases, alias],
        mailFromOrTo,
      }),
    [sessionAliases, mailFromOrTo, alias, OPERATOR],
  );

  const value = useMemo<ViewingAsContextValue>(
    () => ({
      viewingAs: { alias, isOperator: alias === OPERATOR },
      setAlias,
      resetToOperator,
      aliasBuckets,
      aliasesLoading,
      sessionsUnavailable,
      loadAliases,
    }),
    [
      alias,
      OPERATOR,
      setAlias,
      resetToOperator,
      aliasBuckets,
      aliasesLoading,
      sessionsUnavailable,
      loadAliases,
    ],
  );

  // Strict: when the tab is hidden (parent walked away), revert to the
  // operator. Stops a forgotten "reading as X" state from being live the
  // next time someone glances at the laptop.
  useEffect(() => {
    const onVisibilityChange = () => {
      if (document.hidden && alias !== OPERATOR) {
        setAliasState(OPERATOR);
        writeStored(OPERATOR, OPERATOR);
      }
    };
    document.addEventListener('visibilitychange', onVisibilityChange);
    return () => document.removeEventListener('visibilitychange', onVisibilityChange);
  }, [alias, OPERATOR]);

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export function useViewingAs(): ViewingAsContextValue {
  const value = useContext(Context);
  if (value === null) {
    throw new Error('useViewingAs must be inside <ViewingAsProvider>');
  }
  return value;
}
