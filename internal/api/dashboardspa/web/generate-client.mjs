// Bazel entry point for `npm run generate:client` (openapi-ts with
// openapi-ts.config.ts), used by //internal/api/dashboardspa/web:gc_supervisor_client.
//
// It runs openapi-ts exactly as the npm script does, from this directory, then
// copies the generated client to the declared output directory named by
// argv[2], since the config's output path lies in the shared workspace (another
// Bazel package). npm puts node_modules/.bin on PATH for the config's
// `prettier` post-processor; rules_js does not, so a shim provides it.
import { chmodSync, cpSync, mkdtempSync, writeFileSync } from 'node:fs';
import { createRequire } from 'node:module';
import { tmpdir } from 'node:os';
import path from 'node:path';

import { createClient } from '@hey-api/openapi-ts';

const GENERATED_DIR = 'shared/src/generated/gc-supervisor-client';

const outDir = process.argv[2];
if (!outDir) {
  console.error('usage: generate-client.mjs <output-dir>');
  process.exit(2);
}

const require = createRequire(import.meta.url);
const prettierBin = path.join(path.dirname(require.resolve('prettier/package.json')), 'bin', 'prettier.cjs');
const shimDir = mkdtempSync(path.join(tmpdir(), 'openapi-ts-bin-'));
const shim = path.join(shimDir, 'prettier');
writeFileSync(shim, `#!/bin/sh\nexec ${JSON.stringify(process.execPath)} ${JSON.stringify(prettierBin)} "$@"\n`);
chmodSync(shim, 0o755);
process.env.PATH = `${shimDir}${path.delimiter}${process.env.PATH ?? ''}`;

// openapi-ts returns no contexts (and exits 0) when it cannot read its input.
// As the CLI does with no flags: an empty user config loads openapi-ts.config.ts.
const contexts = await createClient({});
if (contexts.length === 0) {
  console.error(`openapi-ts generated nothing (cwd ${process.cwd()}); see its report above`);
  process.exit(1);
}
cpSync(GENERATED_DIR, outDir, { recursive: true });
