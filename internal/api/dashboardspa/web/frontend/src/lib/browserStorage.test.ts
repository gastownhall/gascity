import { afterEach, describe, expect, it, vi } from 'vitest';
import { readBrowserStorage, removeBrowserStorage, writeBrowserStorage } from './browserStorage';

class ThrowingStorage implements Storage {
  get length(): number {
    return 0;
  }
  clear(): void {
    throw new Error('storage blocked');
  }
  getItem(): string | null {
    throw new Error('storage blocked');
  }
  key(): string | null {
    return null;
  }
  removeItem(): void {
    throw new Error('storage blocked');
  }
  setItem(): void {
    throw new Error('storage blocked');
  }
}

function installStorage(property: 'localStorage' | 'sessionStorage', storage: Storage): void {
  Object.defineProperty(window, property, {
    configurable: true,
    value: storage,
  });
  Object.defineProperty(globalThis, property, {
    configurable: true,
    value: storage,
  });
}

const originalLocalStorage = window.localStorage;
const originalSessionStorage = window.sessionStorage;

describe('browser storage wrapper', () => {
  afterEach(() => {
    vi.restoreAllMocks();
    installStorage('localStorage', originalLocalStorage);
    installStorage('sessionStorage', originalSessionStorage);
    document.cookie = 'gascity_admin_csrf=; Max-Age=0; path=/';
  });

  it('distinguishes missing keys from storage failure', () => {
    window.localStorage.clear();

    expect(readBrowserStorage('localStorage', 'missing', 'ThemeContext')).toEqual({
      status: 'missing',
    });

    window.localStorage.setItem('present', 'dark');
    expect(readBrowserStorage('localStorage', 'present', 'ThemeContext')).toEqual({
      status: 'found',
      value: 'dark',
    });
  });

  it('reports read failures to the backend client-error route', () => {
    installStorage('localStorage', new ThrowingStorage());
    document.cookie = 'gascity_admin_csrf=token; path=/';
    const fetchCalls: Array<{ url: unknown; init: RequestInit }> = [];
    const fetchSpy = vi.fn(async (url: unknown, init?: RequestInit) => {
      if (init === undefined) throw new Error('missing fetch init');
      fetchCalls.push({ url, init });
      return new Response(JSON.stringify({ ok: true }), { status: 202 });
    });
    vi.stubGlobal('fetch', fetchSpy);

    expect(readBrowserStorage('localStorage', 'gascity:theme', 'ThemeContext')).toEqual({
      status: 'unavailable',
      error: 'storage blocked',
    });

    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const call = fetchCalls[0];
    if (call === undefined) throw new Error('missing fetch call');
    const { url, init } = call;
    expect(url).toBe('/api/client-errors');
    expect(init.method).toBe('POST');
    expect(init.headers).toMatchObject({
      'Content-Type': 'application/json',
      'X-CSRF-Token': 'token',
    });
    expect(JSON.parse(init.body as string)).toEqual({
      component: 'ThemeContext',
      operation: 'localStorage.getItem',
      message: 'gascity:theme: storage blocked',
    });
  });

  it('reports write and remove failures without throwing', () => {
    installStorage('sessionStorage', new ThrowingStorage());
    const fetchSpy = vi.fn(async () => new Response(JSON.stringify({ ok: true }), { status: 202 }));
    vi.stubGlobal('fetch', fetchSpy);

    expect(
      writeBrowserStorage(
        'sessionStorage',
        'gascity.dashboard.viewingAs',
        'mayor',
        'ViewingAsContext',
      ),
    ).toEqual({ status: 'unavailable', error: 'storage blocked' });
    expect(
      removeBrowserStorage('sessionStorage', 'gascity.dashboard.viewingAs', 'ViewingAsContext'),
    ).toEqual({ status: 'unavailable', error: 'storage blocked' });

    expect(fetchSpy).toHaveBeenCalledTimes(2);
  });
});
