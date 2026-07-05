import { defineConfig, devices } from '@playwright/test';

// Layer B of the dashboard e2e (.dashport-plan/04-e2e.md): a Chromium render
// smoke that drives the REAL built SPA — served by the seeded Go fake supervisor
// (test/dashport/cmd/fakesupervisor) over the shared testdata/dashport corpus —
// and asserts each view renders its seeded content, shows no React error
// boundary, and fires no client-error POST.
//
// The fake supervisor hosts the SPA and its same-origin /v0 + /api surfaces on
// one listener, so no CORS or base-URL override is needed: the browser loads "/"
// (or "/city/{name}/...") from the fake supervisor and its relative fetches
// resolve to the same origin. This is the same server-construction path Layer A
// (test/dashport) drives via api.ServeSeededCity.
//
// The Go binary is prebuilt with -tags integration by the Makefile
// (dashboard-e2e-play) or by test:e2e:build; webServer just launches it on a
// fixed loopback port and waits for "/" to answer.

const PORT = Number(process.env.FAKESUPERVISOR_PORT ?? 8781);
const BASE_URL = `http://127.0.0.1:${PORT}`;

// The compiled fake supervisor and the corpus dir, resolved from the frontend
// workspace (this config's cwd). Overridable so CI or a worktree can point at a
// different build output.
const BINARY =
  process.env.FAKESUPERVISOR_BIN ??
  '../../../../../test/dashport/cmd/fakesupervisor/fakesupervisor';
const CORPUS_DIR =
  process.env.DASHPORT_CORPUS_DIR ?? '../../../../../test/dashport/testdata/dashport';

export default defineConfig({
  testDir: './e2e',
  fullyParallel: true,
  forbidOnly: !!process.env.CI,
  retries: process.env.CI ? 1 : 0,
  // Serial in CI (one shared seeded server); local default (undefined) lets
  // Playwright pick a worker count.
  ...(process.env.CI ? { workers: 1 } : {}),
  reporter: process.env.CI ? [['github'], ['list']] : 'list',
  timeout: 30_000,
  expect: { timeout: 10_000 },
  use: {
    baseURL: BASE_URL,
    trace: 'on-first-retry',
    // The seeded corpus is deterministic, so any console error is a real defect;
    // specs assert on the DOM, but the trace on retry captures the console too.
  },
  projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }],
  webServer: {
    command: `${BINARY} -addr 127.0.0.1:${PORT} -data ${CORPUS_DIR}`,
    url: BASE_URL,
    timeout: 30_000,
    // Local footgun: reuse means a leftover fakesupervisor already on PORT is
    // reused as-is, and it serves the embedded SPA bundle it was built with — so
    // an old process serves a STALE bundle after you rebuild the SPA. If a local
    // run looks wrong, kill the process on PORT (or run `make dashboard-e2e-play`
    // which rebuilds both). CI sets reuseExistingServer=false, so it always
    // launches the freshly built binary and never hits this.
    reuseExistingServer: !process.env.CI,
    stdout: 'pipe',
    stderr: 'pipe',
  },
});
