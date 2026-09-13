import { cleanup, render, within } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import type {
  SessionStructuredBlock,
  SessionStructuredHistory,
  SessionStructuredMessage,
} from 'gas-city-dashboard-shared';
import {
  StructuredBlock,
  StructuredMessage,
  StructuredTranscript,
  PendingInteractionView,
} from './StructuredTranscript';

afterEach(cleanup);

function message(overrides: Partial<SessionStructuredMessage>): SessionStructuredMessage {
  return {
    id: 'msg-1',
    role: 'assistant',
    status: 'final',
    blocks: [],
    ...overrides,
  };
}

describe('StructuredMessage header', () => {
  it('renders all header fields in spec order, omitting empties', () => {
    const { container } = render(
      <StructuredMessage
        message={message({
          role: 'assistant',
          provider: 'claude',
          timestamp: '2026-06-01T00:00:00Z',
          model: 'opus',
          usage: {
            input_tokens: 100,
            output_tokens: 20,
            context_used_tokens: 108,
            context_window_tokens: 200000,
            context_percent: 1,
          },
          status: 'final',
          stop_reason: 'end_turn',
        })}
      />,
    );
    const header = container.querySelector('header');
    expect(header).not.toBeNull();
    const text = header!.textContent ?? '';
    expect(text).toContain('assistant');
    expect(text).toContain('claude');
    expect(text).toContain('opus');
    // Usage line comes verbatim from formatUsage — pin its presence here.
    expect(text).toContain('tokens in 100 out 20 108/200000 1%');
    expect(text).toContain('final');
    expect(text).toContain('end_turn');
  });

  it('omits provider, model, usage, and stop_reason when absent', () => {
    const { container } = render(
      <StructuredMessage message={message({ role: 'user', status: 'final' })} />,
    );
    const text = container.querySelector('header')!.textContent ?? '';
    expect(text).toContain('user');
    expect(text).not.toContain('parent ');
    expect(text).not.toContain('tokens ');
  });
});

describe('StructuredMessage body', () => {
  it('renders user-prompt metadata and suppresses raw text blocks while keeping non-text blocks', () => {
    const { container } = render(
      <StructuredMessage
        message={message({
          role: 'user',
          user_prompt: { text: 'structured prompt text' },
          blocks: [
            { type: 'text', text: 'raw prompt with duplicated content' },
            { type: 'text', text: 'also raw text' },
            { type: 'tool_use', name: 'Read', input: { kind: 'file', file_path: 'x.ts' } },
          ],
        })}
      />,
    );
    const body = container.textContent ?? '';
    // The structured metadata renders.
    expect(body).toContain('prompt: structured prompt text');
    // Every text block is suppressed when prompt metadata rendered (parity with
    // the old renderer's `(promptMetadata || systemEvent) && type==='text'` skip).
    expect(body).not.toContain('raw prompt with duplicated content');
    expect(body).not.toContain('also raw text');
    // Non-text blocks still render.
    expect(body).toContain('Read');
    expect(body).toContain('file: x.ts');
  });

  it('does NOT suppress text blocks when the user_prompt yields no rows', () => {
    // An empty user_prompt produces zero metadata rows, so the old renderer's
    // element-presence gate stays false and text blocks are NOT dropped.
    const { container } = render(
      <StructuredMessage
        message={message({
          role: 'user',
          user_prompt: {},
          blocks: [{ type: 'text', text: 'kept text' }],
        })}
      />,
    );
    expect(container.textContent).toContain('kept text');
  });

  it('does NOT suppress a leading text block when no metadata is present', () => {
    const { container } = render(
      <StructuredMessage
        message={message({ role: 'assistant', blocks: [{ type: 'text', text: 'visible text' }] })}
      />,
    );
    expect(container.textContent).toContain('visible text');
  });

  it('renders system-event metadata', () => {
    const { container } = render(
      <StructuredMessage
        message={message({
          role: 'system',
          system_event: {
            kind: 'error',
            category: 'usage_limit',
            code: 'usage_limit_exceeded',
            message: "You've hit your usage limit.",
          },
          blocks: [],
        })}
      />,
    );
    const text = container.textContent ?? '';
    expect(text).toContain('system');
    expect(text).toContain('kind: error');
    expect(text).toContain('category: usage_limit');
    expect(text).toContain('code: usage_limit_exceeded');
    expect(text).toContain("message: You've hit your usage limit.");
  });
});

describe('StructuredBlock dispatch', () => {
  it('renders a text block', () => {
    const { container } = render(<StructuredBlock block={{ type: 'text', text: 'hello world' }} />);
    expect(container.textContent).toBe('hello world');
  });

  it('renders a thinking block with the [thinking] prefix', () => {
    const { container } = render(
      <StructuredBlock block={{ type: 'thinking', thinking: 'pondering' }} />,
    );
    expect(container.textContent).toBe('[thinking] pondering');
  });

  it('renders a bare [thinking] marker when the text is absent', () => {
    const { container } = render(<StructuredBlock block={{ type: 'thinking' }} />);
    expect(container.textContent).toBe('[thinking]');
  });

  it('renders a tool_use block with name and input rows', () => {
    const block: SessionStructuredBlock = {
      type: 'tool_use',
      name: 'Bash',
      input: { kind: 'command', command: 'npm test' },
    };
    const { container } = render(<StructuredBlock block={block} />);
    const text = container.textContent ?? '';
    expect(text).toContain('Bash');
    expect(text).toContain('kind: command');
    expect(text).toContain('command: npm test');
  });

  it('falls back to a "tool" label when the tool_use block has no name', () => {
    const { container } = render(
      <StructuredBlock block={{ type: 'tool_use', input: { kind: 'command', command: 'ls' } }} />,
    );
    expect(container.textContent).toContain('tool');
    expect(container.textContent).toContain('command: ls');
  });

  it('renders an interaction block as the formatInteraction summary line', () => {
    const block: SessionStructuredBlock = {
      type: 'interaction',
      interaction: {
        kind: 'approval',
        state: 'awaiting_user',
        request_id: 'approval-1',
        action: 'Approve',
        prompt: 'Proceed?',
      },
    };
    const { container } = render(<StructuredBlock block={block} />);
    const text = container.textContent ?? '';
    expect(text).toContain('approval');
    expect(text).toContain('awaiting_user');
    expect(text).toContain('approval-1');
    expect(text).toContain('Approve');
    expect(text).toContain('Proceed?');
  });

  it('renders the closed unknown block variant via the inline-value fallback', () => {
    const { container } = render(
      <StructuredBlock block={{ type: 'unknown', content: 'opaque' }} />,
    );
    // formatInlineValue(block) JSON-stringifies the whole block.
    expect(container.textContent).toContain('unknown');
    expect(container.textContent).toContain('opaque');
  });
});

describe('ToolResultBlock', () => {
  it('renders the kind chip and the joined body', () => {
    const block: SessionStructuredBlock = {
      type: 'tool_result',
      structured: { kind: 'bash', command: 'npm test', stdout: 'ok', exit_code: 0 },
    };
    const { container } = render(<StructuredBlock block={block} />);
    const text = container.textContent ?? '';
    expect(text).toContain('bash');
    expect(text).toContain('result');
    expect(text).toContain('command: npm test');
    expect(text).toContain('stdout: ok');
    expect(text).toContain('exit 0');
  });

  it('renders a diff after the body for an edit result, classed per line', () => {
    const block: SessionStructuredBlock = {
      type: 'tool_result',
      structured: {
        kind: 'edit',
        file_path: 'src/app.ts',
        old_string: 'old line',
        new_string: 'new line',
        patch: '*** Update File: src/app.ts\n@@ -1 +1 @@\n-old line\n+new line',
      },
    };
    const { container } = render(<StructuredBlock block={block} />);
    const text = container.textContent ?? '';
    // Body rows.
    expect(text).toContain('old: old line');
    expect(text).toContain('new: new line');
    // The diff <pre> renders as colorized per-line spans.
    const spans = Array.from(container.querySelectorAll('span'));
    expect(spans.find((s) => s.textContent === '-old line')?.className).toContain('text-warn');
    expect(spans.find((s) => s.textContent === '+new line')?.className).toContain('text-ok');
    expect(spans.find((s) => s.textContent === '*** Update File: src/app.ts')?.className).toContain(
      'text-fg-faint',
    );
  });

  it('applies error styling when is_error is set', () => {
    const block: SessionStructuredBlock = {
      type: 'tool_result',
      is_error: true,
      structured: {
        kind: 'bash',
        error: { category: 'command_failure', message: 'boom' },
        exit_code: 1,
      },
    };
    const { container } = render(<StructuredBlock block={block} />);
    // The wrapping block carries the warn tone.
    expect(container.querySelector('.text-warn')).not.toBeNull();
    expect(container.textContent).toContain('error result');
    expect(container.textContent).toContain('error: boom');
  });

  it('renders a generic result body when the block has no structured payload', () => {
    const { container } = render(
      <StructuredBlock block={{ type: 'tool_result', content: 'plain content' }} />,
    );
    const text = container.textContent ?? '';
    expect(text).toContain('result');
    expect(text).toContain('plain content');
  });
});

describe('ImageBlock', () => {
  it('renders metadata rows and an <img> for a CSP-allowed data URL', () => {
    const block: SessionStructuredBlock = {
      type: 'image',
      file_path: 'screens/shot.png',
      image_url: 'data:image/png;base64,c2hvdA==',
      mime_type: 'image/png',
    };
    const { container } = render(<StructuredBlock block={block} />);
    const text = container.textContent ?? '';
    expect(text).toContain('file: screens/shot.png');
    expect(text).toContain('url: data:image/png;base64,c2hvdA==');
    expect(text).toContain('mime: image/png');

    const img = container.querySelector('img');
    expect(img).not.toBeNull();
    expect(img!.getAttribute('src')).toBe('data:image/png;base64,c2hvdA==');
    expect(img!.getAttribute('alt')).toBe('screens/shot.png');
  });

  it('does not fetch a provider-authored remote image URL blocked by the dashboard CSP', () => {
    const block: SessionStructuredBlock = {
      type: 'image',
      image_url: 'https://attacker.example/tracker.png',
      mime_type: 'image/png',
    };
    const { container } = render(<StructuredBlock block={block} />);

    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('url: https://attacker.example/tracker.png');
  });

  it.each([
    '/\\attacker.example/pixel.png',
    '/\\\\attacker.example/pixel.png',
    '/\\dashboard.example:secret@attacker.example/pixel.png',
  ])('does not render a provider-authored URL that parses as cross-origin: %s', (imageUrl) => {
    const block: SessionStructuredBlock = {
      type: 'image',
      image_url: imageUrl,
      mime_type: 'image/png',
    };
    const { container } = render(<StructuredBlock block={block} />);

    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain(`url: ${imageUrl}`);
  });

  it('renders a same-origin root-relative image URL', () => {
    const block: SessionStructuredBlock = {
      type: 'image',
      image_url: '/screens/shot.png',
      mime_type: 'image/png',
    };
    const { container } = render(<StructuredBlock block={block} />);

    expect(container.querySelector('img')?.getAttribute('src')).toBe('/screens/shot.png');
  });

  it('omits the <img> when there is no image_url', () => {
    const { container } = render(<StructuredBlock block={{ type: 'image', file_path: 'a.png' }} />);
    expect(container.querySelector('img')).toBeNull();
    expect(container.textContent).toContain('file: a.png');
  });
});

describe('PendingInteractionView', () => {
  it('renders the pending interaction rows', () => {
    const { container } = render(
      <PendingInteractionView
        pending={{
          request_id: 'approval-stream',
          kind: 'approval',
          prompt: 'Approve streamed write?',
          options: ['Approve', 'Deny'],
        }}
      />,
    );
    const text = container.textContent ?? '';
    expect(text).toContain('pending');
    expect(text).toContain('interaction');
    expect(text).toContain('kind: approval');
    expect(text).toContain('request: approval-stream');
    expect(text).toContain('prompt: Approve streamed write?');
    expect(text).toContain('options: Approve, Deny');
  });
});

describe('StructuredTranscript', () => {
  const history: SessionStructuredHistory = {
    transcript_stream_id: 'stream-1',
    generation: { id: 'gen-1', observed_at: '2026-06-01T00:00:00Z' },
    cursor: { after_entry_id: 'entry-9', resume_token: 'st1.transcript' },
    continuity: { status: 'continuous', compaction_count: 0 },
    tail_state: { activity: 'idle' },
  };

  it('renders the history envelope before the messages when history is present', () => {
    const { container } = render(
      <StructuredTranscript
        history={history}
        messages={[
          message({
            id: 'm1',
            role: 'assistant',
            blocks: [{ type: 'text', text: 'first message' }],
          }),
        ]}
      />,
    );
    const text = container.textContent ?? '';
    expect(text).toContain('structured session');
    expect(text).toContain('stream: stream-1');
    expect(text).toContain('first message');

    // History must precede the messages list in document order.
    const historyIdx = text.indexOf('stream: stream-1');
    const messageIdx = text.indexOf('first message');
    expect(historyIdx).toBeGreaterThanOrEqual(0);
    expect(historyIdx).toBeLessThan(messageIdx);
  });

  it('renders messages without a history block when history is omitted', () => {
    const { container } = render(
      <StructuredTranscript
        messages={[
          message({ id: 'm1', role: 'assistant', blocks: [{ type: 'text', text: 'alpha' }] }),
          message({ id: 'm2', role: 'user', blocks: [{ type: 'text', text: 'beta' }] }),
        ]}
      />,
    );
    expect(container.textContent).not.toContain('structured session');
    const items = container.querySelectorAll('li');
    expect(items).toHaveLength(2);
    expect(within(items[0] as HTMLElement).getByText('alpha')).toBeTruthy();
    expect(within(items[1] as HTMLElement).getByText('beta')).toBeTruthy();
  });

  it('renders each block type for a message that mixes them', () => {
    const blocks: SessionStructuredBlock[] = [
      { type: 'text', text: 'narration' },
      { type: 'thinking', thinking: 'reasoning' },
      { type: 'tool_use', name: 'Read', input: { kind: 'file', file_path: 'x.ts' } },
      { type: 'tool_result', structured: { kind: 'read', content: 'file body' } },
      { type: 'image', image_url: 'data:image/png;base64,aW1hZ2U=' },
    ];
    const { container } = render(
      <StructuredTranscript messages={[message({ id: 'm1', role: 'assistant', blocks })]} />,
    );
    const text = container.textContent ?? '';
    expect(text).toContain('narration');
    expect(text).toContain('[thinking] reasoning');
    expect(text).toContain('Read');
    expect(text).toContain('file: x.ts');
    expect(text).toContain('file body');
    expect(container.querySelector('img')?.getAttribute('src')).toBe(
      'data:image/png;base64,aW1hZ2U=',
    );
  });
});
