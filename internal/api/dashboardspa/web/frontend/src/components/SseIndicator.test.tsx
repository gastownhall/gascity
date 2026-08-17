import { render, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';
import { SseIndicator } from './SseIndicator';

describe('SseIndicator', () => {
  it('keeps every connection label in a stable-width slot', () => {
    const { rerender } = render(<SseIndicator state="connecting" />);

    expect(screen.getByTitle('SSE stream: connecting').className).toContain('w-28');

    rerender(<SseIndicator state="open" />);
    expect(screen.getByTitle('SSE stream: open').className).toContain('w-28');

    rerender(<SseIndicator state="degraded" />);
    expect(screen.getByTitle('SSE stream: degraded').className).toContain('w-28');

    rerender(<SseIndicator state="closed" />);
    expect(screen.getByTitle('SSE stream: closed').className).toContain('w-28');
  });
});
