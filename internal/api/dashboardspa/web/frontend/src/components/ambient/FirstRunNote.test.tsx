import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it } from 'vitest';
import { FirstRunNote } from './FirstRunNote';

// gascity-dashboard-q89b — first-visit orientation for the ambient home.
// The note must show until dismissed, persist the dismissal per browser,
// and stay inside the editorial register (no maroon, greyscale-readable).

const STORAGE_KEY = 'gascity:home-intro-dismissed';

afterEach(() => {
  cleanup();
  window.localStorage.removeItem(STORAGE_KEY);
});

describe('FirstRunNote', () => {
  it('shows the orientation note on a first visit', () => {
    render(<FirstRunNote />);
    expect(screen.getByTestId('first-run-note')).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Dismiss' })).toBeTruthy();
  });

  it('hides after dismiss and stays hidden on the next mount', () => {
    const first = render(<FirstRunNote />);
    fireEvent.click(screen.getByRole('button', { name: 'Dismiss' }));
    expect(screen.queryByTestId('first-run-note')).toBeNull();
    first.unmount();

    render(<FirstRunNote />);
    expect(screen.queryByTestId('first-run-note')).toBeNull();
    expect(window.localStorage.getItem(STORAGE_KEY)).toBe('1');
  });

  it('carries no maroon mark (One Mark Rule budget stays with the status sentence)', () => {
    const { container } = render(<FirstRunNote />);
    expect(container.querySelectorAll('.text-accent').length).toBe(0);
  });
});
