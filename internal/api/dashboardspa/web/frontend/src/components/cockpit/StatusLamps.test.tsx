import { cleanup, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, describe, expect, it } from 'vitest';

import { StatusLamps, type StatusLamp } from './StatusLamps';

function router(node: ReactNode) {
  return (
    <MemoryRouter future={{ v7_relativeSplatPath: true, v7_startTransition: true }}>
      {node}
    </MemoryRouter>
  );
}

describe('StatusLamps', () => {
  afterEach(() => {
    cleanup();
  });

  it('colors the bulb by tone and never uses the accent mark', () => {
    const lamps: StatusLamp[] = [
      { id: 'ctrl', label: 'controller', value: 'up', tone: 'ok' },
      { id: 'rate', label: 'rate limit', value: 'near', tone: 'warn' },
    ];
    const { container } = render(router(<StatusLamps lamps={lamps} />));
    const bulbs = container.querySelectorAll('[data-testid="lamp-bulb"]');
    expect(bulbs[0]!.className).toContain('text-ok');
    expect(bulbs[1]!.className).toContain('text-warn');
    expect(container.querySelector('.text-accent')).toBeNull();
  });

  it('dims the bulb when the lamp is dim', () => {
    const { container } = render(
      router(<StatusLamps lamps={[{ id: 'a', label: 'idle', value: '—', tone: 'ok', dim: true }]} />),
    );
    expect((container.querySelector('[data-testid="lamp-bulb"]') as HTMLElement).style.opacity).toBe(
      '0.35',
    );
  });

  it('leaves the bulb at full strength when not dim', () => {
    const { container } = render(
      router(<StatusLamps lamps={[{ id: 'a', label: 'live', value: 'ok', tone: 'ok' }]} />),
    );
    expect((container.querySelector('[data-testid="lamp-bulb"]') as HTMLElement).style.opacity).toBe(
      '',
    );
  });

  it('wraps a lamp in a link only when it has an href', () => {
    const { container } = render(
      router(
        <StatusLamps
          lamps={[
            { id: 'a', label: 'controller', value: 'up', tone: 'ok', href: '/health' },
            { id: 'b', label: 'disk', value: 'ok', tone: 'ok' },
          ]}
        />,
      ),
    );
    const links = container.querySelectorAll('a');
    expect(links).toHaveLength(1);
    expect(links[0]!.getAttribute('href')).toBe('/health');
  });

  it('prints the value for each lamp', () => {
    const { container } = render(
      router(<StatusLamps lamps={[{ id: 'a', label: 'queue depth', value: '12', tone: 'warn' }]} />),
    );
    expect(container.querySelector('[data-testid="lamp-value"]')?.textContent).toBe('12');
  });
});
