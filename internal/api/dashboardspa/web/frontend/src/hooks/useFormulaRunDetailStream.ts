import { useEffect, useRef, useState } from 'react';
import type { FormulaRunDetail, RunScopeKind } from 'gas-city-dashboard-shared';
import { errorMessage } from 'gas-city-dashboard-shared';
import { api, decodeFormulaRunDetail } from '../api/client';
import { setCached } from '../api/cache';
import { reportClientError } from '../lib/clientErrorReporting';
import { formulaRunDetailCacheKey } from './useFormulaRunDetail';

// Connection state of the per-run SSE detail stream. 'unavailable' means the
// runtime has no EventSource (SSR/tests without the global, or the stream was
// disabled), so the caller keeps the GET + nudge fallback. 'closed' is reported
// on a stream error so the caller can likewise fall back while the browser
// retries the connection underneath.
export type RunDetailStreamState = 'unavailable' | 'connecting' | 'open' | 'closed';

/**
 * Open the per-run detail SSE stream and push each whole-DTO frame to `onDetail`
 * (a pure renderer of pushed snapshots — no refetch). `onDetail` receives the
 * frame together with the effect-scoped {@link formulaRunDetailCacheKey} the
 * stream was opened for, so the consumer can tag the frame by the run it belongs
 * to rather than by its render-time key — a navigation to a different run can
 * never store this run's frame under the new run's key. Every adopted frame also
 * warms the SWR cache under that same key so a remount paints instantly from the
 * last pushed frame. The stream is the DETAIL refresh channel that replaces the
 * old bead/session nudge re-GET; the initial GET stays as first paint and as the
 * fallback when the stream is unavailable or errors.
 *
 * Lifecycle mirrors useSessionStream: open on mount / param change, close on
 * unmount / change, no reconnect storm (a single EventSource per (runId, scope);
 * the browser handles transient reconnects). A malformed frame is reported once
 * and dropped — the last good frame stays rendered.
 */
export function useFormulaRunDetailStream(
  runId: string | undefined,
  enabled: boolean,
  onDetail: ((detail: FormulaRunDetail, frameKey: string) => void) | undefined,
  scopeKind?: RunScopeKind,
  scopeRef?: string,
): RunDetailStreamState {
  const [state, setState] = useState<RunDetailStreamState>('unavailable');
  const onDetailRef = useRef(onDetail);
  onDetailRef.current = onDetail;
  const malformedReportedRef = useRef(false);
  const cacheKey = formulaRunDetailCacheKey(runId, scopeKind, scopeRef);

  useEffect(() => {
    malformedReportedRef.current = false;
    if (!runId || !enabled || typeof EventSource === 'undefined') {
      setState('unavailable');
      return;
    }
    let cancelled = false;
    setState('connecting');
    const source = new EventSource(api.runDetailStreamUrl(runId), { withCredentials: true });

    source.onopen = () => {
      if (!cancelled) setState('open');
    };
    const onFrame = (event: MessageEvent<string>) => {
      if (cancelled) return;
      const detail = parseDetailFrame(event.data, runId, malformedReportedRef);
      if (detail === null) return;
      // Warm the SWR entry so a remount is cache-warm, then hand the pushed
      // frame to the renderer — zero refetch. Tag it with THIS effect's cacheKey
      // (the run the stream was opened for), not the latest render's, so a frame
      // arriving during an A→B navigation — before this effect's cleanup closes
      // A's source, while onDetailRef already points at B's callback — is still
      // stored under A's key and dropped by B's key comparison.
      setCached(cacheKey, { kind: 'loaded', detail });
      onDetailRef.current?.(detail, cacheKey);
      setState('open');
    };
    source.addEventListener('detail', onFrame);
    source.onerror = () => {
      if (cancelled) return;
      // The browser auto-reconnects an EventSource unless we close it; surface
      // 'closed' so the caller falls back to GET/nudge while it retries.
      setState(source.readyState === EventSource.CLOSED ? 'closed' : 'connecting');
    };

    return () => {
      cancelled = true;
      source.close();
    };
  }, [runId, enabled, cacheKey]);

  return state;
}

function parseDetailFrame(
  data: string,
  runId: string,
  reportedRef: React.MutableRefObject<boolean>,
): FormulaRunDetail | null {
  let parsed: unknown;
  try {
    parsed = JSON.parse(data);
  } catch (err) {
    reportMalformedFrameOnce(runId, reportedRef, err);
    return null;
  }
  try {
    // Same edge validator the GET uses — keep serialization at the edges so a
    // drifted frame fails here rather than deep in the diagram renderer.
    return decodeFormulaRunDetail(parsed, api.runDetailStreamUrl(runId));
  } catch (err) {
    reportMalformedFrameOnce(runId, reportedRef, err);
    return null;
  }
}

function reportMalformedFrameOnce(
  runId: string,
  reportedRef: React.MutableRefObject<boolean>,
  err: unknown,
): void {
  if (reportedRef.current) return;
  reportedRef.current = true;
  void reportClientError({
    component: 'formula-run-detail-stream',
    operation: 'parse stream frame',
    message: `${runId}: ${errorMessage(err)}`,
  });
}
