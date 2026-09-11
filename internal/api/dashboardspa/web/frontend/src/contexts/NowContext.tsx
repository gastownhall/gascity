import { createContext, useContext, useEffect, useState, type ReactNode } from 'react';

// gascity-dashboard-kb3 (PRD §4 R9): the L0 ambient home derives its
// staleness tier + age display + favicon-hysteresis input from a single
// 1s wall-clock tick. Centralising the tick in a context means every
// consumer re-renders together (cheap for ~10 lanes) and we only pay one
// setInterval per page — the alternative of each hook running its own
// interval was the explicit anti-pattern flagged in the Phase 1 review.

const NowContext = createContext<number | null>(null);

interface NowProviderProps {
  children: ReactNode;
  /** Default 1000ms; overridden in tests to drive the clock deterministically. */
  intervalMs?: number;
}

export function NowProvider({ children, intervalMs = 1000 }: NowProviderProps) {
  const [now, setNow] = useState<number>(() => Date.now());

  useEffect(() => {
    const handle = window.setInterval(() => {
      setNow(Date.now());
    }, intervalMs);
    return () => {
      window.clearInterval(handle);
    };
  }, [intervalMs]);

  return <NowContext.Provider value={now}>{children}</NowContext.Provider>;
}

export function useNow(): number {
  const value = useContext(NowContext);
  if (value === null) {
    throw new Error('useNow must be called inside a NowProvider.');
  }
  return value;
}
