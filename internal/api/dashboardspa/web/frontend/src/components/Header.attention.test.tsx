import { cleanup, render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { AttentionProvider } from '../attention/context';
import type { AttentionContributor, AttentionItem } from '../attention/compose';
import { ThemeProvider } from '../contexts/ThemeContext';
import { ViewingAsProvider } from '../contexts/ViewingAsContext';
import { Header } from './Header';

vi.mock('../api/client', () => ({
  api: {
    config: vi.fn(async () => ({
      cityName: 'test-city',
      defaultView: null,
      enabledModules: [],
      readOnly: false,
    })),
  },
}));

vi.mock('../supervisor/client', () => ({
  supervisorApi: () => ({
    listCities: vi.fn(async () => ({
      items: [{ name: 'test-city', path: '/srv/test-city', running: true }],
      total: 1,
    })),
  }),
}));

describe('Header attention indicators', () => {
  beforeEach(() => {
    Object.defineProperty(window, 'matchMedia', {
      configurable: true,
      value: vi.fn().mockImplementation((query: string) => ({
        addEventListener: vi.fn(),
        addListener: vi.fn(),
        dispatchEvent: vi.fn(),
        matches: false,
        media: query,
        onchange: null,
        removeEventListener: vi.fn(),
        removeListener: vi.fn(),
      })),
    });
  });

  afterEach(() => {
    cleanup();
  });

  it('rolls up highest severity and count from the shared attention model', async () => {
    renderHeader([
      contributor('runs', [item('run-1', 'runs', 'attention'), item('run-2', 'runs', 'watch')]),
      contributor('mail', [item('mail-1', 'mail', 'watch')]),
      contributor('activity', [item('activity-1', 'activity', 'attention')]),
    ]);

    expect((await screen.findByLabelText('Runs: 2 attention items')).textContent).toBe('2');
    expect(screen.getByLabelText('Mail: 1 watch item').textContent).toBe('1');
    expect(screen.getByLabelText('Activity: 1 attention item').textContent).toBe('1');
    expect(screen.queryByLabelText(/Agents:/)).toBeNull();
  });
});

function renderHeader(contributors: readonly AttentionContributor[]) {
  return render(
    <ThemeProvider>
      <ViewingAsProvider>
        <AttentionProvider contributors={contributors}>
          <MemoryRouter
            initialEntries={['/runs']}
            future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
          >
            <Header />
          </MemoryRouter>
        </AttentionProvider>
      </ViewingAsProvider>
    </ThemeProvider>,
  );
}

function contributor(
  domain: AttentionItem['domain'],
  items: readonly AttentionItem[],
): AttentionContributor {
  return {
    id: `${domain}-test`,
    domain,
    getItems: () => items,
  };
}

function item(
  id: string,
  domain: AttentionItem['domain'],
  severity: AttentionItem['severity'],
): AttentionItem {
  return {
    id,
    domain,
    severity,
    title: id,
    href: `/${domain}`,
    current: true,
    actionable: false,
  };
}
