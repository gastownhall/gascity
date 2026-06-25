import { Button } from '../Button';

export interface AgentDirectivesError {
  status?: number;
  kind?: string;
  message: string;
}

export function AgentDirectives({
  alias,
  prompt,
  loading,
  error,
  onRefresh,
}: {
  alias: string;
  prompt: string | null;
  loading: boolean;
  error: AgentDirectivesError | null;
  onRefresh: () => void;
}) {
  const isNotFound = error?.status === 404 || error?.kind === 'not_found';
  const charsLabel =
    prompt !== null
      ? `${prompt.length.toLocaleString()} chars`
      : loading
        ? 'loading'
        : error !== null
          ? '—'
          : '·';

  return (
    <section className="mt-12">
      <header className="flex items-baseline justify-between mb-4">
        <h2 className="text-label uppercase tracking-wider text-fg-faint">Directives</h2>
        <div className="flex items-baseline gap-3">
          <span className="text-label uppercase tracking-wider text-fg-faint tnum">
            {charsLabel}
          </span>
          <Button size="sm" tone="quiet" onClick={onRefresh} disabled={loading}>
            {loading ? 'Refreshing' : 'Refresh'}
          </Button>
        </div>
      </header>
      {loading && prompt === null && error === null ? (
        <p className="text-body text-fg-muted italic">Loading directives.</p>
      ) : isNotFound ? (
        <p className="text-body text-warn">
          Agent <code className="text-fg">{alias}</code> has no entry in city config.
        </p>
      ) : error !== null ? (
        <p className="text-body text-accent" role="alert">
          {error.status ? `${error.status} ` : ''}
          {error.message}
        </p>
      ) : prompt !== null ? (
        <pre className="text-body whitespace-pre-wrap leading-relaxed text-fg overflow-x-auto max-h-[60vh] overflow-y-auto">
          {prompt}
        </pre>
      ) : null}
    </section>
  );
}
