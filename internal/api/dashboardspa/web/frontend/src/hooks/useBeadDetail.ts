import { useEffect, useState } from 'react';
import { useNow } from '../contexts/NowContext';
import { fetchSupervisorBead, type SupervisorBead } from '../supervisor/beadReads';
import { SupervisorApiError } from '../supervisor/client';

// Shared fetch state for a single bead's detail surface
// (gascity-dashboard-6frc). The BeadDetailModal renders this from both the
// Beads board and the AgentDetail assigned-beads list, so the "fetch by id
// when the cached row lacks a description" logic lives here once rather
// than in each surface.

export interface BeadDetailState {
  bead: SupervisorBead | null;
  loading: boolean;
  error: string | null;
  /**
   * The bead id resolved to a 404 — it no longer exists in the supervisor.
   * Distinct from `error` so callers can render a calm "resolved or removed"
   * state for a stale deep-link (gascity-dashboard-sg9o / -jrs0) rather than a
   * hard failure. A pruned or since-closed-and-swept bead is an expected,
   * non-alarming outcome when an attention deep-link outlives its target.
   */
  notFound: boolean;
  /** Live clock (ms) for RelatedEntities staleness / "as of". */
  now: number;
}

/**
 * Loads `beadId` while `active`. If `initialBead` already carries a
 * description (the freshness signal — it's the thing the detail surface
 * exists to show), the fetch is skipped. Relative ages use the app-wide
 * NowProvider clock so detail surfaces stay consistent with the page.
 */
export function useBeadDetail(
  active: boolean,
  beadId: string | null,
  initialBead: SupervisorBead | null = null,
): BeadDetailState {
  const [bead, setBead] = useState<SupervisorBead | null>(initialBead);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [notFound, setNotFound] = useState(false);
  const now = useNow();

  useEffect(() => {
    if (!active || !beadId) return;
    if (initialBead && initialBead.id === beadId && initialBead.description !== undefined) {
      setBead(initialBead);
      setError(null);
      setNotFound(false);
      return;
    }
    setBead(initialBead?.id === beadId ? initialBead : null);
    setLoading(true);
    setError(null);
    setNotFound(false);
    let cancelled = false;
    void (async () => {
      try {
        const result = await fetchSupervisorBead(beadId);
        if (!cancelled) setBead(result);
      } catch (err) {
        if (cancelled) return;
        if (err instanceof SupervisorApiError && err.status === 404) {
          // The deep-linked bead is gone (pruned, or closed-and-swept since
          // the attention queue cached it). Surface as a calm absence, not an
          // error banner.
          setNotFound(true);
        } else {
          setError(formatSupervisorError(err));
        }
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [active, beadId, initialBead]);

  return { bead, loading, error, notFound, now };
}

function formatSupervisorError(err: unknown): string {
  if (err instanceof SupervisorApiError) {
    return err.status === undefined ? err.message : `${err.status} ${err.message}`;
  }
  if (err instanceof Error) return err.message;
  return 'fetch failed';
}
