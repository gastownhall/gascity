import { formatRelative } from '../../hooks/time';
import { PROMPT_INJECTION_NOTICE } from '../../lib/constants';
import type { SupervisorMailItem } from '../../supervisor/mailReads';

export function AgentChatThread({
  messages,
  loading,
  error,
  now,
}: {
  messages: ReadonlyArray<SupervisorMailItem>;
  loading: boolean;
  error: string | null;
  now: number;
}) {
  return (
    <section className="mt-12">
      <header className="flex items-baseline justify-between mb-4">
        <h2 className="text-label uppercase tracking-wider text-fg-faint">Chat thread</h2>
        <span className="text-label uppercase tracking-wider text-fg-faint tnum">
          {loading ? '·' : messages.length}
        </span>
      </header>
      <p className="text-label uppercase tracking-wider text-fg-faint mb-4">
        <span className="text-accent">▲ {PROMPT_INJECTION_NOTICE}</span>
      </p>
      {loading ? (
        <p className="text-body text-fg-muted italic">Loading messages.</p>
      ) : error !== null ? (
        <p className="text-body text-accent" role="alert">
          {error}
        </p>
      ) : messages.length === 0 ? (
        <p className="text-body text-fg-muted italic">
          No messages between operator and this agent.
        </p>
      ) : (
        <ul className="space-y-6">
          {messages.map((m) => (
            <li key={m.id} className="space-y-2 pb-4 border-b border-rule last:border-0">
              <header className="flex items-baseline justify-between gap-3">
                <div className="text-label uppercase tracking-wider text-fg-muted truncate">
                  <span className="text-fg font-medium">{m.from}</span>
                  <span className="mx-1.5 text-fg-faint">→</span>
                  <span>{m.to}</span>
                </div>
                <span className="text-label uppercase tracking-wider text-fg-faint tnum shrink-0">
                  {formatRelative(m.created_at, now)}
                </span>
              </header>
              {m.subject && <p className="text-body font-medium text-fg">{m.subject}</p>}
              <pre className="text-body whitespace-pre-wrap leading-relaxed text-fg overflow-x-auto">
                {m.body}
              </pre>
            </li>
          ))}
        </ul>
      )}
    </section>
  );
}
