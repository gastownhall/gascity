import { cleanup, render } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { StructuredStreamState } from '../hooks/useStructuredSessionStream';
import type { SessionStreamState } from '../hooks/useSessionStream';
import { StructuredLivePeek } from './StructuredLivePeek';

const mockUseStructured = vi.hoisted(() => vi.fn());
const mockUseSessionStream = vi.hoisted(() => vi.fn());

vi.mock('../hooks/useStructuredSessionStream', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../hooks/useStructuredSessionStream')>();
  return { ...actual, useStructuredSessionStream: mockUseStructured };
});

vi.mock('../hooks/useSessionStream', async (importOriginal) => {
  const actual = await importOriginal<typeof import('../hooks/useSessionStream')>();
  return { ...actual, useSessionStream: mockUseSessionStream };
});

const readyState: StructuredStreamState = {
  status: 'ready',
  stream: { status: 'open' },
  result: {
    provider: 'claude',
    template: 'mayor',
    history: {
      transcript_stream_id: 'stream-1',
      generation: { id: 'gen-1' },
      cursor: { resume_token: 'st1.live-peek' },
      continuity: { status: 'continuous' },
      tail_state: { activity: 'idle' },
    },
    items: [
      {
        kind: 'message',
        message: {
          id: 'm1',
          role: 'assistant',
          status: 'final',
          blocks: [{ type: 'text', text: 'hello world' }],
        },
      },
    ],
    activity: 'idle',
  },
};

const conversationState: SessionStreamState = {
  status: 'ready',
  stream: { status: 'open' },
  result: {
    id: 's1',
    template: 'mayor',
    provider: 'claude',
    format: 'conversation',
    turns: [{ role: 'assistant', text: 'conversation turn body' }],
    total_chars: 22,
    captured_at: '2026-06-30T00:00:00Z',
    truncated: false,
  },
};

describe('StructuredLivePeek', () => {
  beforeEach(() => {
    mockUseStructured.mockReset();
    mockUseSessionStream.mockReset();
    mockUseSessionStream.mockReturnValue({ status: 'idle', stream: { status: 'idle' } });
  });

  afterEach(cleanup);

  it('renders the structured transcript when ready', () => {
    mockUseStructured.mockReturnValue(readyState);
    const { container } = render(<StructuredLivePeek sessionId="s1" stream />);
    const text = container.textContent ?? '';
    expect(text).toContain('hello world');
    expect(text).toContain('stream: stream-1'); // history envelope
    expect(mockUseSessionStream).not.toHaveBeenCalled(); // no conversation fallback
    // StructuredMessage is itself an <li>; the body must not double-wrap it.
    expect(container.querySelector('li li')).toBeNull();
    const transcript = container.querySelector('ol');
    expect(transcript?.querySelector(':scope > li')).not.toBeNull();
    expect(transcript?.getAttribute('aria-live')).toBe('polite');
    expect(transcript?.getAttribute('aria-relevant')).toBe('additions text');
  });

  it('renders pending interactions appended to the transcript', () => {
    mockUseStructured.mockReturnValue({
      ...readyState,
      result: {
        ...readyState.result,
        items: [
          ...readyState.result.items,
          { kind: 'pending', pending: { request_id: 'req-9', kind: 'tool_approval' } },
        ],
      },
    } satisfies StructuredStreamState);
    const { container } = render(<StructuredLivePeek sessionId="s1" stream />);
    expect(container.textContent ?? '').toContain('request: req-9');
  });

  it('falls back to the conversation peek when structured is unavailable', () => {
    mockUseStructured.mockReturnValue({ status: 'unavailable', stream: { status: 'idle' } });
    mockUseSessionStream.mockReturnValue(conversationState);
    const { container } = render(<StructuredLivePeek sessionId="s1" stream />);
    expect(container.textContent ?? '').toContain('conversation turn body');
  });

  it('shows a loading line while fetching', () => {
    mockUseStructured.mockReturnValue({ status: 'loading', stream: { status: 'connecting' } });
    const { getByText } = render(<StructuredLivePeek sessionId="s1" stream />);
    expect(getByText('Fetching transcript.')).toBeTruthy();
  });

  it('surfaces a load failure as an alert', () => {
    mockUseStructured.mockReturnValue({
      status: 'failed',
      error: 'peek failed',
      stream: { status: 'idle' },
    });
    const { getByRole } = render(<StructuredLivePeek sessionId="s1" stream />);
    expect(getByRole('alert').textContent).toBe('peek failed');
  });

  it('renders nothing when idle (no session)', () => {
    mockUseStructured.mockReturnValue({ status: 'idle', stream: { status: 'idle' } });
    const { container } = render(<StructuredLivePeek sessionId={null} stream={false} />);
    expect(container.textContent).toBe('');
  });
});
