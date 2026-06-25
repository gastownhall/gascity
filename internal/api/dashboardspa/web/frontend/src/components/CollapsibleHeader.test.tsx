import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { CollapsibleHeader } from './CollapsibleHeader';

describe('CollapsibleHeader', () => {
  afterEach(() => {
    cleanup();
  });

  it('renders one accessible toggle button with a collapsed glyph', () => {
    render(
      <CollapsibleHeader collapsed onToggle={() => {}} className="custom-header">
        {({ glyph }) => (
          <>
            <span>{glyph}Regression</span>
            <span>3 items</span>
          </>
        )}
      </CollapsibleHeader>,
    );

    const button = screen.getByRole('button', { name: /regression.*3 items/i });
    expect(button.getAttribute('aria-expanded')).toBe('false');
    expect(button.classList.contains('custom-header')).toBe(true);
    expect(button.querySelector('[aria-hidden="true"]')?.textContent).toBe('▾');
  });

  it('calls onToggle when clicked', () => {
    const onToggle = vi.fn();
    render(
      <CollapsibleHeader collapsed={false} onToggle={onToggle}>
        {({ glyph }) => <span>{glyph}Open section</span>}
      </CollapsibleHeader>,
    );

    fireEvent.click(screen.getByRole('button', { name: /open section/i }));

    expect(onToggle).toHaveBeenCalledTimes(1);
  });
});
