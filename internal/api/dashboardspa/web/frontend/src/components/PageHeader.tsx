import type { ReactNode } from 'react';

// The opener of every route. A Display heading naming the view, a
// one-line synopsis of state, and an optional right-aligned meta slot
// (SSE indicator, refresh control, etc.). Sets the rhythm for the
// page: generous space above + below, no card, no border.

interface PageHeaderProps {
  title: string;
  synopsis?: ReactNode;
  meta?: ReactNode;
  className?: string;
}

export function PageHeader({ title, synopsis, meta, className = '' }: PageHeaderProps) {
  return (
    <header
      className={`grid grid-cols-1 items-start gap-x-6 gap-y-4 mb-10 md:grid-cols-[minmax(0,1fr)_auto] md:items-end ${className}`}
    >
      <div className="min-w-0 space-y-2">
        <h1 className="text-display font-semibold tracking-tighter text-fg leading-[1.05]">
          {title}
        </h1>
        {synopsis && <p className="text-body text-fg-muted max-w-prose">{synopsis}</p>}
      </div>
      {meta && (
        <div className="flex flex-wrap items-center gap-4 text-label uppercase tracking-wider md:justify-end">
          {meta}
        </div>
      )}
    </header>
  );
}
