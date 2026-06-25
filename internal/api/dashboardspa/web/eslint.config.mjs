import js from '@eslint/js';
import globals from 'globals';
import importPlugin from 'eslint-plugin-import';
import react from 'eslint-plugin-react';
import reactHooks from 'eslint-plugin-react-hooks';
import tseslint from 'typescript-eslint';

const tsFiles = ['backend/**/*.ts', 'shared/**/*.ts', 'frontend/src/**/*.{ts,tsx}'];

const typedConfigs = [...tseslint.configs.recommended].map((config) => ({
  ...config,
  files: tsFiles,
}));

const typeAwareSourceFiles = [
  'backend/src/**/*.ts',
  'frontend/src/**/*.{ts,tsx}',
  'shared/src/**/*.ts',
];

// Colocated backend test files (PR-B1+) live under backend/src/**/*.test.ts.
// Pre-PR-B1 backend tests lived in backend/test/ and were never subject to
// type-aware rules (the glob above only matched non-test src). Preserve
// that behaviour for the migrated colocated tests so the relocation isn't
// entangled with a global lint-rule expansion (one change, one purpose).
// Frontend tests stay type-aware as they always were under frontend/src/.
const backendColocatedTestGlobs = ['backend/src/**/*.test.ts', 'backend/src/**/fixtures/**'];

// Names of the module-isolation subdirs under views/modules/. Exported so
// the drift-detector test can read it and assert it ⊇ actual subdirs of
// `{backend,frontend}/src/views/modules/`. A new module added to the
// registry without being added here would silently lose cross-import
// protection — the drift-detector closes that window.
// Defined ABOVE `export default` so `moduleIsolationConfigs()` (called
// during default-export evaluation) doesn't TDZ-throw when it reads it.
export const MODULE_ISOLATION_NAMES = ['maintainer', 'health', 'activity'];

export default tseslint.config(
  {
    ignores: [
      '**/dist/**',
      '**/dist-test/**',
      '**/node_modules/**',
      '**/coverage/**',
      '**/.claude/**',
      '**/.gc/**',
    ],
  },
  {
    linterOptions: {
      reportUnusedDisableDirectives: 'error',
      reportUnusedInlineConfigs: 'error',
    },
  },
  {
    files: ['**/*.{js,mjs,cjs}'],
    ...js.configs.recommended,
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      globals: {
        ...globals.browser,
        ...globals.node,
      },
    },
  },
  ...typedConfigs,
  {
    files: tsFiles,
    plugins: {
      import: importPlugin,
    },
    rules: {
      '@typescript-eslint/no-unused-vars': [
        'error',
        {
          argsIgnorePattern: '^_',
          varsIgnorePattern: '^_',
          caughtErrorsIgnorePattern: '^_',
        },
      ],
      'import/first': 'error',
      'no-console': 'error',
    },
  },
  {
    files: ['backend/src/logging.ts', 'frontend/src/test/**/*.ts', '**/*.test.{ts,tsx}'],
    rules: {
      'no-console': 'off',
    },
  },
  {
    files: typeAwareSourceFiles,
    ignores: ['shared/src/**/*.test.ts', ...backendColocatedTestGlobs],
    languageOptions: {
      ecmaVersion: 'latest',
      sourceType: 'module',
      parserOptions: {
        project: [
          './backend/tsconfig.test.json',
          './frontend/tsconfig.test.json',
          './shared/tsconfig.json',
        ],
        tsconfigRootDir: import.meta.dirname,
      },
      globals: {
        ...globals.browser,
        ...globals.node,
      },
    },
    rules: {
      '@typescript-eslint/consistent-type-imports': [
        'error',
        {
          fixStyle: 'inline-type-imports',
        },
      ],
      '@typescript-eslint/no-floating-promises': [
        'error',
        {
          ignoreVoid: true,
        },
      ],
      '@typescript-eslint/no-misused-promises': [
        'error',
        {
          checksVoidReturn: {
            attributes: false,
          },
        },
      ],
      '@typescript-eslint/switch-exhaustiveness-check': 'error',
      '@typescript-eslint/restrict-template-expressions': [
        'error',
        {
          allowBoolean: true,
          allowNumber: true,
          allowNullish: true,
        },
      ],
    },
  },
  {
    files: ['frontend/src/**/*.{ts,tsx}'],
    plugins: {
      react,
      'react-hooks': reactHooks,
    },
    settings: {
      react: { version: 'detect' },
    },
    rules: {
      'react/no-danger': 'error',
      'react-hooks/exhaustive-deps': 'error',
      'react-hooks/rules-of-hooks': 'error',
    },
  },
  {
    files: ['**/*.test.{ts,tsx}'],
    rules: {
      '@typescript-eslint/no-non-null-assertion': 'off',
      '@typescript-eslint/no-empty-function': 'off',
    },
  },
  // Modular-dashboard cross-module isolation (PR-C / bead 9yj.5 / PRD §1 /
  // premortem #4). Each `views/modules/<X>/` directory is a self-contained
  // module: it may import shared/, the host's lib/logging/config layer, and
  // anything OUTSIDE views/modules/, but NEVER files belonging to a sibling
  // module. Cross-module collaboration must happen through CityContext /
  // ModuleResources, not direct imports — otherwise the "delete a module by
  // removing it from ALL_MODULES / ALL_VIEWS" promise breaks.
  //
  // Implemented as per-side overrides because the file glob has to live in
  // `files:` and the forbidden pattern depends on which side (backend vs
  // frontend) the importer is on. Colocated `*.test.ts(x)` files inside a
  // module ARE part of that module — they are exempted from the
  // restriction because test files legitimately reach for sibling helpers
  // and the rule's grouping cannot express "any module except mine" via a
  // single pattern. The lint rule enforces production-code isolation; the
  // tests verify the same boundary at runtime.
  ...moduleIsolationConfigs(),
);

/**
 * Build per-module ESLint zones for the backend and frontend module trees
 * (PR-C / bead 9yj.5). Each module subdirectory under
 * `<side>/src/views/modules/<X>/` becomes its own zone:
 *
 *   - Files inside `<X>/` may import their own siblings (no rule fires).
 *   - Files inside `<X>/` may import anything OUTSIDE `views/modules/`
 *     (shared/, lib/, logging, config — the common surface).
 *   - Files inside `<X>/` may NOT import from any other `views/modules/<Y>/`
 *     — the `from`/`except` zone says "anywhere in modules/ EXCEPT my own
 *     dir is off-limits."
 *
 * Implemented via `import/no-restricted-paths` (not plain
 * `no-restricted-imports`) because the latter matches literal import
 * strings, and a relative sibling import (`../<other>/foo.js`) bypasses
 * a glob like `**\/views/modules/*\/**`. `no-restricted-paths` resolves
 * the import to an absolute path before matching, so it catches every
 * shape of cross-module reference.
 *
 * Module DIRECTORIES are the unit of isolation: a single-file module
 * (e.g. `backend/src/views/modules/health.module.ts`) lives at the
 * top of the tree, has no siblings of its own, and cannot import into
 * a sibling subdirectory by accident — so it does not need a zone of
 * its own. Adding a new module subdir means adding it to MODULE_DIRS
 * below (the registry edit is the design-review checkpoint anyway,
 * per PRD premortem #6).
 */
function moduleIsolationConfigs() {
  const MESSAGE =
    'cross-module import forbidden — modules talk via CityContext / ModuleResources, not direct imports. See specs/architecture/module-author-checklist.md.';

  // PR-C Phase-4 HIGH fix: extend the `no-restricted-imports` patterns to
  // cover ALL practical depths from a module subdir, closing the bypass
  // the reviewer found where a 2-up reach (`maintainer/fixtures/x.ts`
  // → `../../health.module.js`) slipped past the original patterns. We
  // initially tried `import/no-restricted-paths` (depth-independent by
  // design) but it requires the import to RESOLVE first, and the default
  // node resolver doesn't swap TypeScript's ESM `.js` → `.ts` — so the
  // rule silently skips on every TS file in this codebase. Installing
  // `eslint-import-resolver-typescript` would fix that but adds a dep
  // for a problem we can pin down with one more pattern depth.
  //
  // Pattern shapes (literal-string minimatch, applied per importer):
  //   `./X/**`           — flat-file module reaches sibling SUBDIR module
  //   `./X.module*`      — flat-file module reaches sibling FLAT module
  //   `../X/**`          — 1-up: subdir module reaches sibling SUBDIR
  //   `../X.module*`     — 1-up: subdir module reaches sibling FLAT
  //   `../../X/**`       — 2-up: nested subdir reaches sibling SUBDIR (e.g. fixtures/)
  //   `../../X.module*`  — 2-up: nested subdir reaches sibling FLAT
  //   `../../../X/**`    — 3-up: deeply nested (services/foo/bar.ts)
  //   `../../../X.module*`
  //   `../../modules/X/**` + `../../modules/X.module*` — explicit modules/-segment forms
  //
  // The `../../X` form (no `modules/` segment) is what the reviewer's
  // bypass exploited. The drift-detector test in `backend/test/views-registry
  // .test.ts` asserts MODULE_ISOLATION_NAMES ⊇ actual subdirs/flat-files of
  // views/modules/ so a new module added to the registry without being
  // added here surfaces immediately.
  const MODULE_NAMES = MODULE_ISOLATION_NAMES;

  function patternsFor() {
    const groups = [];
    for (const name of MODULE_NAMES) {
      groups.push(`./${name}/**`);
      groups.push(`./${name}.module*`);
      groups.push(`../${name}/**`);
      groups.push(`../${name}.module*`);
      groups.push(`../../${name}/**`);
      groups.push(`../../${name}.module*`);
      groups.push(`../../../${name}/**`);
      groups.push(`../../../${name}.module*`);
      groups.push(`../../modules/${name}/**`);
      groups.push(`../../modules/${name}.module*`);
    }
    return [{ group: groups, message: MESSAGE }];
  }

  return [
    {
      files: ['backend/src/views/modules/**/*.ts'],
      ignores: ['backend/src/views/modules/**/*.test.ts'],
      rules: {
        'no-restricted-imports': ['error', { patterns: patternsFor() }],
      },
    },
    {
      files: ['frontend/src/views/modules/**/*.{ts,tsx}'],
      ignores: ['frontend/src/views/modules/**/*.test.{ts,tsx}'],
      rules: {
        'no-restricted-imports': ['error', { patterns: patternsFor() }],
      },
    },
  ];
}
