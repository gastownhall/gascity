// Bazel entry point for the Playwright render smoke (`npm run test:e2e`), used
// by //internal/api/dashboardspa/web/frontend:playwright_test.
//
// Bazel hands the test runfiles locations (rlocation paths) of what
// playwright.config.ts reads from the environment: the seeded fakesupervisor
// binary, its corpus, the pinned chrome-headless-shell, and the shared
// libraries the shell needs that the host lacks. This resolves them to
// absolute paths and runs `playwright test` from this directory, as the npm
// script does.
import { spawnSync } from 'node:child_process';
import { createRequire } from 'node:module';
import path from 'node:path';

function runfile(envName) {
  const rlocation = process.env[envName]?.split(' ')[0];
  const root = process.env.TEST_SRCDIR ?? process.env.RUNFILES_DIR;
  if (!rlocation || !root) {
    console.error(`playwright-bazel: ${envName} and TEST_SRCDIR must be set`);
    process.exit(2);
  }
  return path.join(root, rlocation);
}

const chrome = runfile('CHROME_HEADLESS_SHELL');
const runtimeLibs = path.dirname(runfile('CHROMIUM_RUNTIME_LIBNSS3'));
const env = {
  ...process.env,
  // playwright.config.ts: serial, no retries beyond CI's one, fresh server.
  CI: '1',
  FAKESUPERVISOR_BIN: runfile('FAKESUPERVISOR'),
  // Every corpus file sits in testdata/dashport; the first names the directory.
  DASHPORT_CORPUS_DIR: path.dirname(runfile('DASHPORT_CORPUS_FILES')),
  // <browsers>/chromium_headless_shell-<rev>/chrome-headless-shell-linux64/chrome-headless-shell
  PLAYWRIGHT_BROWSERS_PATH: path.resolve(chrome, '..', '..', '..'),
  // The host-dependency probe shells out to ldd against the host's library
  // set; the libraries come from LD_LIBRARY_PATH here instead.
  PLAYWRIGHT_SKIP_VALIDATE_HOST_REQUIREMENTS: '1',
  LD_LIBRARY_PATH: [runtimeLibs, path.join(runtimeLibs, 'nss'), process.env.LD_LIBRARY_PATH]
    .filter(Boolean)
    .join(path.delimiter),
  // Reports and traces go to the test's undeclared outputs.
  PLAYWRIGHT_HTML_OUTPUT_DIR: path.join(process.env.TEST_UNDECLARED_OUTPUTS_DIR ?? '.', 'playwright-report'),
};

const require = createRequire(import.meta.url);
const cli = require.resolve('@playwright/test/cli');
const args = [cli, 'test', '--output', path.join(process.env.TEST_UNDECLARED_OUTPUTS_DIR ?? '.', 'test-results')];
const result = spawnSync(process.execPath, args, { env, stdio: 'inherit' });
if (result.error) {
  console.error(`playwright-bazel: ${result.error.message}`);
  process.exit(1);
}
process.exit(result.status ?? 1);
