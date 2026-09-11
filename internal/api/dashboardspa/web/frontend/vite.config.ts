import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Single-port deploy: the gc supervisor serves the SPA, the typed `/v0/*`
// API, `/health`, and the host-side `/api/*` plane on one loopback listener
// (default 127.0.0.1:8372). In dev, Vite proxies those prefixes to that
// listener so the browser stays same-origin both in dev and prod — no
// `/gc-supervisor` transport proxy, which keeps the Host-allowlist + Origin
// check + CSP simple.
// DEV_BACKEND_TARGET lets an isolated worktree stack proxy to its own
// supervisor port (pair with `vite --port <n>`), so the snap harness can
// drive it via SNAP_BASE without touching the primary :5174/:8372 pair.
const DEFAULT_BACKEND_TARGET = 'http://127.0.0.1:8372';
const LOOPBACK_HOSTS = new Set(['127.0.0.1', 'localhost', '::1']);

// The supervisor binds 127.0.0.1 only and must never be exposed, so a custom
// dev proxy target must still resolve to loopback. Validate at config load — a
// non-loopback or malformed DEV_BACKEND_TARGET fails loudly here rather than
// silently proxying dev traffic off-host.
function resolveBackendTarget(): string {
  const raw = process.env.DEV_BACKEND_TARGET;
  if (raw === undefined) return DEFAULT_BACKEND_TARGET;
  let hostname: string;
  try {
    hostname = new URL(raw).hostname;
  } catch {
    throw new Error(`DEV_BACKEND_TARGET is not a valid URL: ${JSON.stringify(raw)}`);
  }
  // URL parsing wraps IPv6 hosts in brackets ([::1]); strip them before compare.
  const normalized = hostname.replace(/^\[|\]$/g, '');
  if (!LOOPBACK_HOSTS.has(normalized)) {
    throw new Error(
      `DEV_BACKEND_TARGET must resolve to loopback (127.0.0.1, localhost, or ::1); got ${JSON.stringify(hostname)}`,
    );
  }
  return raw;
}

export const BACKEND_TARGET = resolveBackendTarget();

interface ProxyRequest {
  hasHeader(name: string): boolean;
  setHeader(name: string, value: string): void;
}
type BackendDevProxy = {
  on(event: 'proxyReq', listener: (proxyReq: ProxyRequest) => void): void;
};

export function configureBackendDevProxy(proxy: BackendDevProxy): void {
  proxy.on('proxyReq', (proxyReq) => {
    if (proxyReq.hasHeader('origin')) {
      proxyReq.setHeader('Origin', BACKEND_TARGET);
    }
  });
}

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5174,
    strictPort: true,
    host: '127.0.0.1',
    proxy: {
      // Host-side dashboard plane served by the supervisor.
      '/api': {
        target: BACKEND_TARGET,
        // changeOrigin rewrites the *Host* header to match the supervisor's
        // host:port so the Host-allowlist (127.0.0.1, localhost) passes.
        changeOrigin: true,
        // changeOrigin does NOT rewrite the *Origin* request header — that
        // still arrives as http://127.0.0.1:5174 (Vite's own origin) and
        // would 403 against the supervisor's originCheck allow-list of
        // {http://127.0.0.1:8372, http://localhost:8372}. Rewrite it
        // explicitly so dev write requests (POST/PATCH/DELETE) clear the
        // allow-list. In prod the supervisor serves the SPA, so the browser
        // sends Origin: http://127.0.0.1:8372 natively and this code path
        // doesn't apply (gascity-dashboard-oi7).
        configure: configureBackendDevProxy,
      },
      // Typed supervisor API. Served same-origin by the supervisor in prod;
      // proxied here in dev so the browser addresses `/v0/...` directly.
      '/v0': {
        target: BACKEND_TARGET,
        changeOrigin: true,
        // Same Origin rewrite as /api so dev supervisor mutations clear the
        // originCheck allow-list.
        configure: configureBackendDevProxy,
      },
      // Supervisor liveness probe.
      '/health': {
        target: BACKEND_TARGET,
        changeOrigin: true,
        configure: configureBackendDevProxy,
      },
    },
  },
  build: {
    outDir: 'dist',
    // No prod source maps — an externally-fronted dist must not ship readable
    // source. Keep false; see specs/architecture/exposure.md.
    sourcemap: false,
    emptyOutDir: true,
  },
});
