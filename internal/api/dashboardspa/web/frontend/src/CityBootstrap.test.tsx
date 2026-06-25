import { cleanup, render, screen, waitFor } from '@testing-library/react';
import { afterAll, afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { CityBootstrap } from './CityBootstrap';
import type { CityInfo } from 'gas-city-dashboard-shared/gc-supervisor';

// A lightweight App stand-in: the bootstrap test asserts that the router tree
// mounts, not the whole dashboard, so a marker beats pulling in every provider.
vi.mock('./App', () => ({
  App: () => <div data-testid="app-mounted">app</div>,
}));

const listCities = vi.fn();

vi.mock('./supervisor/client', () => ({
  supervisorApi: () => ({ listCities }),
}));

function city(name: string, running = true): CityInfo {
  return { name, path: `/srv/${name}`, running };
}

const replaceSpy = vi.fn();
const assignSpy = vi.fn();
const originalLocationDescriptor = Object.getOwnPropertyDescriptor(window, 'location');

// jsdom's window.location members are non-configurable, so we cannot spy on
// replace/assign in place. Replace the whole object with a full stub that
// carries every field BrowserRouter reads (href/origin/pathname/search/hash)
// plus our navigation spies. The runaway-render bug this once triggered is
// fixed in the component (parsed is memoized), so a static stub is safe.
function setPath(pathname: string): void {
  const origin = 'http://127.0.0.1';
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: {
      href: `${origin}${pathname}`,
      origin,
      protocol: 'http:',
      host: '127.0.0.1',
      hostname: '127.0.0.1',
      port: '',
      pathname,
      search: '',
      hash: '',
      replace: replaceSpy,
      assign: assignSpy,
      reload: vi.fn(),
    },
  });
}

describe('CityBootstrap', () => {
  beforeEach(() => {
    listCities.mockReset();
    replaceSpy.mockReset();
    assignSpy.mockReset();
  });

  afterEach(() => {
    cleanup();
  });

  afterAll(() => {
    // Restore jsdom's real location so the swapped stub never leaks into
    // another test file sharing this worker.
    if (originalLocationDescriptor !== undefined) {
      Object.defineProperty(window, 'location', originalLocationDescriptor);
    }
  });

  it('mounts the app when the URL city is registered (happy path)', async () => {
    setPath('/city/alpha/');
    listCities.mockResolvedValue({ items: [city('alpha'), city('beta')], total: 2 });

    render(<CityBootstrap />);

    expect(await screen.findByTestId('app-mounted')).toBeTruthy();
  });

  it('redirects a bare path to the first registered city', async () => {
    setPath('/');
    listCities.mockResolvedValue({ items: [city('alpha')], total: 1 });

    render(<CityBootstrap />);

    await waitFor(() => {
      expect(replaceSpy).toHaveBeenCalledWith('/city/alpha/');
    });
  });

  it('renders the unknown-city screen with links when the URL city is absent', async () => {
    setPath('/city/ghost/');
    listCities.mockResolvedValue({ items: [city('alpha'), city('beta', false)], total: 2 });

    render(<CityBootstrap />);

    expect(
      await screen.findByText(/City .*ghost.* is not registered on this supervisor/),
    ).toBeTruthy();
    const alphaLink = screen.getByRole('link', { name: 'alpha' });
    expect(alphaLink.getAttribute('href')).toBe('/city/alpha/');
    expect(screen.getByRole('link', { name: 'beta' }).getAttribute('href')).toBe('/city/beta/');
    expect(screen.queryByTestId('app-mounted')).toBeNull();
  });

  it('mounts the app on a listCities network error so a blip does not lock the operator out', async () => {
    setPath('/city/alpha/');
    listCities.mockRejectedValue(new Error('network down'));

    render(<CityBootstrap />);

    expect(await screen.findByTestId('app-mounted')).toBeTruthy();
  });

  it('renders the empty state with a register command on a fresh install', async () => {
    setPath('/');
    listCities.mockResolvedValue({ items: [], total: 0 });

    render(<CityBootstrap />);

    expect(
      await screen.findByText('No cities are registered on this supervisor.'),
    ).toBeTruthy();
    expect(screen.getByText('gc init ~/my-city')).toBeTruthy();
    expect(screen.getByRole('link', { name: /getting-started guide/ })).toBeTruthy();
  });

  it('recovers from the error state via Retry without a full reload', async () => {
    setPath('/');
    listCities.mockRejectedValueOnce(new Error('boom'));

    render(<CityBootstrap />);

    expect(await screen.findByText('Could not load cities.')).toBeTruthy();

    // The next fetch succeeds; Retry must re-run it (not reload the page).
    listCities.mockResolvedValue({ items: [city('alpha')], total: 1 });
    screen.getByRole('button', { name: 'Retry' }).click();

    await waitFor(() => {
      expect(replaceSpy).toHaveBeenCalledWith('/city/alpha/');
    });
    expect(listCities).toHaveBeenCalledTimes(2);
  });
});
