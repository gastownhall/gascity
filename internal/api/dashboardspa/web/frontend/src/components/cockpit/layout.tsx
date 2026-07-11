import type { ReactNode } from 'react';

// Presentational layout chrome shared by the cockpit home bands: a column
// heading, a dial cell (an instrument plus an optional microcopy line), and the
// microcopy line itself. No state, no data — pure composition helpers so the
// route stays hooks + band JSX.

/** ColumnHeading is the small uppercase caption above a band column. */
export function ColumnHeading({ children }: { children: ReactNode }) {
  return <p className="mb-2 text-label uppercase tracking-wider text-fg-faint">{children}</p>;
}

/**
 * DialCell centres an instrument and, when a source is degraded, prints a small
 * microcopy line beneath it (the anti-Norse rule: the dial stays mounted at zero
 * rather than collapsing to a sentence).
 */
export function DialCell({
  children,
  note,
  noteTestId,
}: {
  children: ReactNode;
  note?: string | undefined;
  noteTestId?: string | undefined;
}) {
  return (
    <div className="flex flex-col items-center gap-1">
      {children}
      {note !== undefined && <Microcopy testId={noteTestId}>{note}</Microcopy>}
    </div>
  );
}

/** Microcopy is the small italic per-instrument degraded-state caption. */
export function Microcopy({ children, testId }: { children: ReactNode; testId?: string | undefined }) {
  return (
    <p className="text-label italic text-fg-faint" {...(testId ? { 'data-testid': testId } : {})}>
      {children}
    </p>
  );
}
