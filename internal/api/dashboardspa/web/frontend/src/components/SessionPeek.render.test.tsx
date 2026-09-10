import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { SessionPeekContent } from './SessionPeek';
import type { SessionTranscriptView } from '../supervisor/sessionReads';

// gascity-dashboard-5e5v / xl07: raw terminal control bytes leaked into the
// rendered peek transcript (Stephanie saw `... then proceed.^[`). These tests
// mount the real render path and assert the artifacts never reach the DOM.

afterEach(cleanup);

function viewWithTurn(text: string): SessionTranscriptView {
  return {
    turns: [{ role: 'assistant', text }],
    total_chars: text.length,
    captured_at: '2026-06-03T00:00:00Z',
    truncated: false,
  } as SessionTranscriptView;
}

describe('SessionPeekContent — terminal control stripping', () => {
  it('renders the leaked transcript without escape artifacts', () => {
    const dirty =
      '... then proceed.\x1b\x1b]0;evil-title\x07 colour \x1b[31mred\x1b[0m tail\x9chere';
    const { container } = render(
      <SessionPeekContent loading={false} error={null} result={viewWithTurn(dirty)} />,
    );
    const rendered = container.textContent ?? '';

    // The visible, printable content survives.
    expect(rendered).toContain('... then proceed.');
    expect(rendered).toContain('colour');
    expect(rendered).toContain('red');
    expect(rendered).toContain('tailhere');

    // No raw control bytes reach the DOM.
    expect(rendered).not.toContain('\x1b');
    expect(rendered).not.toContain('\x9c');
    expect(rendered).not.toContain('evil-title');
    // The literal `^[` glyph form must not appear either.
    expect(rendered).not.toContain('^[');
    // SGR parameter text must not leak as visible characters.
    expect(rendered).not.toContain('[31m');
    expect(rendered).not.toContain('[0m');
  });

  it('shows an expand button for a non-empty transcript', () => {
    render(<SessionPeekContent loading={false} error={null} result={viewWithTurn('hello')} />);
    expect(screen.getByRole('button', { name: /expand/i })).toBeTruthy();
  });

  it('does not show an expand button while loading', () => {
    render(<SessionPeekContent loading={true} error={null} result={null} />);
    expect(screen.queryByRole('button', { name: /expand/i })).toBeNull();
  });

  it('colorizes the surviving SGR sequence via ansi_up classes', () => {
    const dirty = 'plain \x1b[31mred-text\x1b[0m done';
    const { container } = render(
      <SessionPeekContent loading={false} error={null} result={viewWithTurn(dirty)} />,
    );
    // ansi_up with use_classes emits ansi-* class spans for SGR colour.
    expect(container.querySelector('[class*="ansi-"]')).not.toBeNull();
    expect(container.textContent).toContain('red-text');
  });
});

// A turn list that pages backwards prepends older turns. Keyed by array index,
// React rebinds every existing turn to a different DOM node when that happens,
// so per-node state (scroll position, selection) follows the wrong turn. Now
// that the API reports a stable entry id, the list keys on it.
describe('SessionPeekContent — turn identity across backward paging', () => {
  function viewWithTurns(turns: { id: string; text: string }[]): SessionTranscriptView {
    return {
      turns: turns.map((t) => ({ id: t.id, role: 'assistant', text: t.text })),
      total_chars: 0,
      captured_at: '2026-06-03T00:00:00Z',
      truncated: false,
    } as SessionTranscriptView;
  }

  it('keeps a turn bound to its own DOM node when an older turn is prepended', () => {
    const { container, rerender } = render(
      <SessionPeekContent
        loading={false}
        error={null}
        result={viewWithTurns([
          { id: 'a', text: 'alpha' },
          { id: 'b', text: 'beta' },
        ])}
      />,
    );

    const betaNode = Array.from(container.querySelectorAll('li')).find((li) =>
      li.textContent?.includes('beta'),
    );
    expect(betaNode).toBeDefined();

    rerender(
      <SessionPeekContent
        loading={false}
        error={null}
        result={viewWithTurns([
          { id: 'older', text: 'older' },
          { id: 'a', text: 'alpha' },
          { id: 'b', text: 'beta' },
        ])}
      />,
    );

    expect(container.querySelectorAll('li')).toHaveLength(3);
    // Keyed by id this node still holds beta; keyed by index it would now hold alpha.
    expect(betaNode?.textContent).toContain('beta');
  });
});
