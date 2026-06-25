import { useCallback, useEffect, useRef, useState } from 'react';
import { errorMessage } from 'gas-city-dashboard-shared';
import {
  readBrowserStorage,
  writeBrowserStorage,
  type BrowserStorageArea,
} from '../lib/browserStorage';
import { reportClientError } from '../lib/clientErrorReporting';

interface UsePersistedCollapseSetOptions {
  key: string;
  component: string;
  area?: BrowserStorageArea;
}

interface PersistedCollapseSet {
  isCollapsed: (id: string) => boolean;
  toggle: (id: string) => void;
  setExact: (ids: Iterable<string>) => void;
}

export function usePersistedCollapseSet({
  key,
  component,
  area = 'localStorage',
}: UsePersistedCollapseSetOptions): PersistedCollapseSet {
  const [collapsed, setCollapsed] = useState<ReadonlySet<string>>(() =>
    loadCollapsedIds(area, key, component),
  );
  const didMount = useRef(false);

  useEffect(() => {
    if (!didMount.current) {
      didMount.current = true;
      return;
    }
    setCollapsed(loadCollapsedIds(area, key, component));
  }, [area, key, component]);

  const persist = useCallback(
    (next: ReadonlySet<string>) => {
      writeBrowserStorage(area, key, JSON.stringify(Array.from(next)), component);
    },
    [area, key, component],
  );

  const toggle = useCallback(
    (id: string) => {
      setCollapsed((prev) => {
        const next = new Set(prev);
        if (next.has(id)) next.delete(id);
        else next.add(id);
        persist(next);
        return next;
      });
    },
    [persist],
  );

  const setExact = useCallback(
    (ids: Iterable<string>) => {
      const next = new Set(ids);
      persist(next);
      setCollapsed(next);
    },
    [persist],
  );

  const isCollapsed = useCallback((id: string) => collapsed.has(id), [collapsed]);

  return { isCollapsed, toggle, setExact };
}

function loadCollapsedIds(
  area: BrowserStorageArea,
  key: string,
  component: string,
): ReadonlySet<string> {
  const stored = readBrowserStorage(area, key, component);
  if (stored.status !== 'found') return new Set();
  try {
    const parsed = JSON.parse(stored.value);
    if (
      !Array.isArray(parsed) ||
      !parsed.every((value): value is string => typeof value === 'string')
    ) {
      throw new Error('expected an array of strings');
    }
    return new Set(parsed);
  } catch (err) {
    reportStorageParseFailure(area, key, component, err);
    return new Set();
  }
}

function reportStorageParseFailure(
  area: BrowserStorageArea,
  key: string,
  component: string,
  err: unknown,
): void {
  void reportClientError({
    component,
    operation: `${area}.parse`,
    message: `${key}: ${errorMessage(err)}`,
  });
}
