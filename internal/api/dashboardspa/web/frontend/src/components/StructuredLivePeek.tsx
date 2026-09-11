import { useStructuredSessionStream, type StructuredStreamState } from '../hooks/useStructuredSessionStream';
import { PROMPT_INJECTION_NOTICE } from '../lib/constants';
import { LiveSessionPeek, streamBadge } from './LiveSessionPeek';
import { StatusBadge } from './StatusBadge';
import { TranscriptBox } from './TranscriptBox';
import {
  PendingInteractionView,
  StructuredHistory,
  StructuredMessage,
} from './structured/StructuredTranscript';

// Live peek for PR #3718's format=structured transcripts. Composes the
// structured snapshot + SSE tail (useStructuredSessionStream) with the Slice 3b
// renderers, and degrades to the conversation LiveSessionPeek when the server
// has no structured transcript for this session — so every session still
// renders something. Mirrors LiveSessionPeek's chrome (connection badge +
// TranscriptBox) so the two peeks look the same.

interface StructuredLivePeekProps {
  /** Session to peek. Null renders nothing (idle). */
  sessionId: string | null;
  /** Open the live SSE tail. False = one-shot snapshot only. */
  stream: boolean;
  /** Show the connection badge. Default true. */
  showBadge?: boolean;
}

export function StructuredLivePeek({ sessionId, stream, showBadge = true }: StructuredLivePeekProps) {
  const state = useStructuredSessionStream(sessionId, stream);

  // The server returned a non-structured transcript — render the conversation
  // peek instead so the view never goes blank.
  if (state.status === 'unavailable') {
    return <LiveSessionPeek sessionId={sessionId} stream={stream} showBadge={showBadge} showCaption />;
  }
  if (state.status === 'idle') return null;

  const badge = streamBadge(state.stream);
  return (
    <div className="space-y-4">
      {showBadge && (
        <div className="flex justify-end">
          <StatusBadge
            tone={badge.tone}
            label={badge.label}
            title={`Session stream: ${state.stream.status}`}
            className="text-label uppercase tracking-wider"
          />
        </div>
      )}
      <StructuredPeekBody state={state} />
    </div>
  );
}

function StructuredPeekBody({ state }: { state: StructuredStreamState }) {
  if (state.status === 'loading') {
    return <p className="text-fg-muted italic">Fetching transcript.</p>;
  }
  if (state.status === 'failed') {
    return (
      <p className="text-accent" role="alert">
        {state.error}
      </p>
    );
  }
  if (state.status !== 'ready') return null;

  const { result } = state;
  if (result.items.length === 0 && result.history === null) {
    return <p className="text-fg-muted italic">No structured transcript yet.</p>;
  }

  return (
    <TranscriptBox>
      <div className="space-y-6">
        <p className="text-label uppercase tracking-wider text-warn">▲ {PROMPT_INJECTION_NOTICE}</p>
        {result.history !== null && <StructuredHistory history={result.history} />}
        <ol className="space-y-5" aria-live="polite" aria-relevant="additions text">
          {result.items.map((item) =>
            // StructuredMessage renders its own <li>; only the pending view (a
            // <div>) needs wrapping so every <ol> child is an <li>.
            item.kind === 'message' ? (
              <StructuredMessage key={`m-${item.message.id}`} message={item.message} />
            ) : (
              <li key={`p-${item.pending.request_id}`}>
                <PendingInteractionView pending={item.pending} />
              </li>
            ),
          )}
        </ol>
      </div>
    </TranscriptBox>
  );
}
