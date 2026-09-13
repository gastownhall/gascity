import { cleanup, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { PartialDataNotice } from './PartialDataNotice';

describe('PartialDataNotice', () => {
  afterEach(() => cleanup());

  it('renders a shared warn-toned degraded-source status line', () => {
    render(<PartialDataNotice label="roster partial" title="one backend unavailable" />);

    const status = screen.getByRole('status');
    expect(status.textContent).toBe('roster partial');
    expect(status.getAttribute('title')).toBe('one backend unavailable');
    expect(status.className).toContain('text-warn');
  });

  it('renders nothing when show is false', () => {
    render(<PartialDataNotice show={false} label="runs partial" title="one rig unavailable" />);

    expect(screen.queryByRole('status')).toBeNull();
  });

  it('pairs an optional leading glyph with the word (DESIGN.md status = glyph + word)', () => {
    render(<PartialDataNotice glyph="◐" label="runs partial" title="one rig unavailable" />);

    const status = screen.getByRole('status');
    expect(status.textContent).toContain('◐');
    expect(status.textContent).toContain('runs partial');
  });
});
