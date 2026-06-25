// @vitest-environment node

import { describe, expect, it, vi } from 'vitest';
import viteConfig, { BACKEND_TARGET, configureBackendDevProxy } from '../vite.config';

describe('vite dev proxy config', () => {
  it('rewrites Origin for the dashboard api, typed supervisor api, and health writes', () => {
    const proxy = viteConfig.server?.proxy;
    expect(proxy).toBeTypeOf('object');
    if (typeof proxy !== 'object' || proxy === null || Array.isArray(proxy)) {
      throw new Error('vite proxy config missing');
    }

    for (const prefix of ['/api', '/v0', '/health'] as const) {
      expect(proxy[prefix]).toMatchObject({
        target: BACKEND_TARGET,
        changeOrigin: true,
        configure: configureBackendDevProxy,
      });
    }
  });

  it('rewrites browser Origin to the backend origin when present', () => {
    let listener: ((req: ProxyRequestDouble) => void) | undefined;
    configureBackendDevProxy({
      on(event, next) {
        expect(event).toBe('proxyReq');
        listener = next;
      },
    });
    const proxyReq = {
      hasHeader: vi.fn((name: string) => name.toLowerCase() === 'origin'),
      setHeader: vi.fn(),
    };

    if (listener === undefined) throw new Error('proxyReq listener not registered');
    listener(proxyReq);

    expect(proxyReq.setHeader).toHaveBeenCalledWith('Origin', BACKEND_TARGET);
  });

  it('leaves originless requests untouched', () => {
    let listener: ((req: ProxyRequestDouble) => void) | undefined;
    configureBackendDevProxy({
      on(_event, next) {
        listener = next;
      },
    });
    const proxyReq = {
      hasHeader: vi.fn(() => false),
      setHeader: vi.fn(),
    };

    if (listener === undefined) throw new Error('proxyReq listener not registered');
    listener(proxyReq);

    expect(proxyReq.setHeader).not.toHaveBeenCalled();
  });
});

interface ProxyRequestDouble {
  hasHeader(name: string): boolean;
  setHeader(name: string, value: string): void;
}
