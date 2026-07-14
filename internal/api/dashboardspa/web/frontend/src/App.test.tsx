import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, useLocation } from 'react-router-dom';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { App } from './App';
import { ThemeProvider } from './contexts/ThemeContext';

vi.mock('./api/client', () => ({
  api: {
    config: vi.fn(async () => ({
      cityName: 'test-city',
      defaultView: null,
      enabledModules: [],
      readOnly: false,
      operatorAlias: 'stephanie',
      operatorWireAlias: 'human',
      decisionLabel: 'needs/stephanie',
    })),
  },
}));

vi.mock('./supervisor/client', () => ({
  supervisorApi: () => ({
    listCities: vi.fn(async () => ({
      items: [{ name: 'test-city', path: '/srv/gc/test-city', running: true }],
      total: 1,
    })),
  }),
}));

vi.mock('./routes/Runs', () => ({
  RunsPage: () => <h1>Runs route</h1>,
}));

vi.mock('./routes/AmbientHome', () => ({
  AmbientHomePage: () => <h1>Ambient home route</h1>,
}));

function LocationProbe() {
  const location = useLocation();
  return <output data-testid="pathname">{location.pathname}</output>;
}

function renderAt(path: string) {
  return render(
    <ThemeProvider>
      <MemoryRouter
        initialEntries={[path]}
        future={{ v7_relativeSplatPath: true, v7_startTransition: true }}
      >
        <App />
        <LocationProbe />
      </MemoryRouter>
    </ThemeProvider>,
  );
}

describe('App routes', () => {
  afterEach(() => {
    cleanup();
  });

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

  it.each(['/workflows', '/kanban'])('%s is not a compatibility redirect', async (path) => {
    renderAt(path);

    await waitFor(() => {
      expect(screen.getByTestId('pathname').textContent).toBe(path);
    });
    expect(screen.queryByRole('heading', { name: 'Runs route' })).toBeNull();
    expect(screen.getByRole('heading', { name: 'Page not found' })).toBeTruthy();
  });

  it('/runs still renders the run list route', async () => {
    renderAt('/runs');

    expect(await screen.findByRole('heading', { name: 'Runs route' })).toBeTruthy();
    expect(screen.getByTestId('pathname').textContent).toBe('/runs');
  });

  it('/ renders the OSS ambient home fallback', async () => {
    renderAt('/');

    expect(await screen.findByRole('heading', { name: 'Ambient home route' })).toBeTruthy();
    expect(screen.getByTestId('pathname').textContent).toBe('/');
  });
});
