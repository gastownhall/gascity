import { act, cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { TranscriptBox } from './TranscriptBox';

// jsdom has no matchMedia; the overlay's prefers-reduced-motion check needs it.
// Mirrors the stub in Header.attention.test.tsx.
function stubMatchMedia(matches: boolean): void {
  Object.defineProperty(window, 'matchMedia', {
    configurable: true,
    value: vi.fn().mockImplementation((query: string) => ({
      matches,
      media: query,
      onchange: null,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      addListener: vi.fn(),
      removeListener: vi.fn(),
      dispatchEvent: vi.fn(),
    })),
  });
}

beforeEach(() => stubMatchMedia(false));
afterEach(cleanup);

describe('TranscriptBox', () => {
  it('renders children in a scrollable collapsed pane with an expand button', () => {
    render(<TranscriptBox>transcript content</TranscriptBox>);
    expect(screen.getByText('transcript content')).toBeTruthy();
    expect(screen.getByRole('button', { name: /expand/i })).toBeTruthy();
  });

  it('opens an expanded overlay when expand is clicked', () => {
    render(<TranscriptBox>content</TranscriptBox>);
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    expect(screen.getByRole('dialog', { name: /transcript/i })).toBeTruthy();
    expect(screen.getByRole('button', { name: /collapse/i })).toBeTruthy();
    // Content is still visible in the overlay.
    expect(screen.getByText('content')).toBeTruthy();
  });

  it('collapses back when the collapse button is clicked', () => {
    render(<TranscriptBox>content</TranscriptBox>);
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    expect(screen.queryByRole('dialog')).toBeTruthy();
    fireEvent.click(screen.getByRole('button', { name: /collapse/i }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(screen.getByRole('button', { name: /expand/i })).toBeTruthy();
  });

  it('collapses when the backdrop is clicked', () => {
    render(<TranscriptBox>content</TranscriptBox>);
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    // The backdrop has aria-hidden so we query by the element preceding the dialog.
    const backdrop = document.querySelector('[aria-hidden="true"]');
    expect(backdrop).not.toBeNull();
    fireEvent.click(backdrop!);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('collapses on Escape key in capture phase without closing a parent dialog', () => {
    // Simulate a parent modal's bubble-phase Escape handler.
    const parentClose = vi.fn();
    document.addEventListener('keydown', parentClose);

    render(<TranscriptBox>content</TranscriptBox>);
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    expect(screen.queryByRole('dialog')).toBeTruthy();

    // Dispatch Escape — the TranscriptBox capture handler should fire and
    // stop propagation before the bubble-phase parentClose.
    act(() => {
      document.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }));
    });

    expect(screen.queryByRole('dialog')).toBeNull();
    // Parent bubble handler must NOT have fired.
    expect(parentClose).not.toHaveBeenCalled();

    document.removeEventListener('keydown', parentClose);
  });

  it('shows the expanded overlay fully visible (no enter animation) when prefers-reduced-motion is set', () => {
    // DESIGN.md §6: with reduced motion the overlay must appear at its final
    // opacity/scale synchronously rather than tweening in via rAF.
    stubMatchMedia(true);
    render(<TranscriptBox>content</TranscriptBox>);
    fireEvent.click(screen.getByRole('button', { name: /expand/i }));
    const dialog = screen.getByRole('dialog', { name: /transcript/i });
    expect(dialog.className).toContain('opacity-100');
    expect(dialog.className).toContain('scale-100');
    expect(dialog.className).toContain('motion-reduce:transition-none');
  });
});
