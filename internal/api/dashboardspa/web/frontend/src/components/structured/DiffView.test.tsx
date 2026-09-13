import { cleanup, render } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { DiffView } from './DiffView';

afterEach(cleanup);

// One representative diff exercising every diffLineKind branch. The `*** Update
// File:` separator is what `patchTextFromHunks` emits, so it stands in for the
// file-header kind; the `@@` line is the hunk header; `+`/`-` are add/del; the
// unprefixed line is context.
const DIFF = ['*** Update File: src/app.ts', '@@ -1 +1 @@', '-old line', '+new line', ' context line'].join('\n');

describe('DiffView', () => {
  it('renders one span per line, each classed by its diff kind', () => {
    const { container } = render(<DiffView text={DIFF} />);
    const spans = Array.from(container.querySelectorAll('span'));
    expect(spans).toHaveLength(5);

    const byText = (needle: string) => spans.find((s) => s.textContent === needle);

    expect(byText('*** Update File: src/app.ts')?.className).toContain('text-fg-faint');
    expect(byText('@@ -1 +1 @@')?.className).toContain('text-fg-muted');
    expect(byText('-old line')?.className).toContain('text-warn');
    expect(byText('+new line')?.className).toContain('text-ok');
    expect(byText(' context line')?.className).toContain('text-fg');
  });

  it('classifies +++ / --- file headers as file, not add/del', () => {
    const { container } = render(<DiffView text={'--- a/x.ts\n+++ b/x.ts'} />);
    const spans = Array.from(container.querySelectorAll('span'));
    expect(spans).toHaveLength(2);
    for (const span of spans) {
      expect(span.className).toContain('text-fg-faint');
      expect(span.className).not.toContain('text-warn');
      expect(span.className).not.toContain('text-ok');
    }
  });

  it('normalizes CRLF and keeps blank lines as empty spans', () => {
    const { container } = render(<DiffView text={'+added\r\n\r\n context'} />);
    const spans = Array.from(container.querySelectorAll('span'));
    expect(spans).toHaveLength(3);
    expect(spans[0]?.textContent).toBe('+added');
    expect(spans[1]?.textContent).toBe('');
    expect(spans[2]?.textContent).toBe(' context');
  });

  it('renders inside a single pre element', () => {
    const { container } = render(<DiffView text={'+x'} />);
    expect(container.querySelectorAll('pre')).toHaveLength(1);
  });

  it('preserves newlines in the pre textContent so a copied diff keeps its lines', () => {
    const { container } = render(<DiffView text={DIFF} />);
    expect(container.querySelector('pre')?.textContent).toBe(DIFF);
  });
});
