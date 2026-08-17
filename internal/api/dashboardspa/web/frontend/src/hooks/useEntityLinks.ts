import { useEffect, useState } from 'react';
import type { EntityLinkView } from 'gas-city-dashboard-shared';
import { formatApiError } from '../api/client';
import { loadSupervisorEntityLinks } from '../supervisor/entityLinks';

// Fetch the bead-ID linked view for a focus ref (gascity-dashboard-j4x).
//
// Relations change slowly, so this is static-with-implicit-refresh: it
// refetches only when the ref changes (OQ#6 — the calm choice; no SSE
// auto-refresh wired). A null ref yields an idle state (no fetch), which
// lets a caller hold the hook before its entity has loaded.

export interface UseEntityLinks {
  view: EntityLinkView | null;
  loading: boolean;
  error: string | null;
}

export function useEntityLinks(ref: string | null): UseEntityLinks {
  const [view, setView] = useState<EntityLinkView | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (ref === null || ref.length === 0) {
      setView(null);
      setError(null);
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    void (async () => {
      try {
        const result = await loadSupervisorEntityLinks(ref);
        if (!cancelled) setView(result);
      } catch (err) {
        if (cancelled) return;
        setError(formatApiError(err, 'related entities failed'));
        setView(null);
      } finally {
        if (!cancelled) setLoading(false);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [ref]);

  return { view, loading, error };
}
