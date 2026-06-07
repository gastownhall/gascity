# Release Gate: ga-1l42us reaper split schema

Branch: `release/ga-1l42us-reaper-split-schema`
Base: `origin/main` at `7d17197bcd42629bddd788d2be1463f2d9e6dc7c`
Release commit before gate checklist: `b19a3db191f504bd9b57f15cbac358f4a377069c`
Reviewed source commit: `be98b23c317517b6b9147005dac2c0e68b817bd0`
Source branch: `work/ga-pftmco-adopt-fix`
Source bead: `ga-fa7rkq`
Deploy bead: `ga-1l42us`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria table plus the bead-specific release handoff.

## Scope

The maintenance reaper no longer checks the removed `depends_on_id` generated
column before running per-database cleanup. The gate now requires the current
split dependency target columns, and every maintenance query that follows
dependency edges uses `depends_on_issue_id`, `depends_on_wisp_id`, and
`depends_on_external` as appropriate.

Changed in this branch:

- `examples/gastown/packs/maintenance/assets/scripts/reaper.sh`: updates the
  dependency schema gate, the reusable close-edge predicate, and all per-site
  dependency joins/selects to use split target columns.
- `examples/gastown/packs/maintenance/formulas/mol-dog-reaper.toml`: keeps the
  formula SQL and prose aligned with the split dependency schema.
- `examples/gastown/maintenance_scripts_test.go`: updates mock Dolt stubs and
  assertions, and adds coverage that split-schema reaper queries do not use
  `d.depends_on_id`.
- `examples/gastown/maintenance_scripts_dolt_integration_test.go`: updates the
  integration fixture DDL and seed rows for the split dependency columns.

The release commit is the same patch as the reviewed commit after rebase onto
current `origin/main`:

- `git patch-id --stable < <(git show be98b23c3)` -> `0dd7e23d00f9139a429d4da774849041c315748c`
- `git patch-id --stable < <(git show b19a3db19)` -> `0dd7e23d00f9139a429d4da774849041c315748c`

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-fa7rkq` is closed with close reason `pass` and notes headed `REVIEWER VERDICT: PASS` for commit `be98b23c3` on `work/ga-pftmco-adopt-fix`. The deploy handoff `ga-1l42us` also records reviewed + passed status. |
| 2 | Acceptance criteria met | PASS | `reaper.sh` has no `depends_on_id` hits and requires `depends_on_issue_id`, `depends_on_wisp_id`, and `depends_on_external` in `has_dependency_target_column`. The reusable close-edge predicate and the test constant are byte-identical. The formula SQL and integration fixtures use the split dependency schema. Remaining `depends_on_id` references are confined to bd CLI JSON projection tests and legacy error text, not maintenance SQL. |
| 3 | Tests pass | PASS | See test log below. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-fa7rkq` report no blockers and no HIGH findings; deploy handoff includes no open finding references. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate; final cleanliness is verified after committing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` returned `rc=0`; `git diff --check $(git merge-base HEAD origin/main)..HEAD` returned no whitespace/errors. |
| 7 | Single feature theme | PASS | One subsystem/theme: the Gas Town maintenance reaper and its tests/formula now target the current split dependency schema. The commit touches only four `examples/gastown` maintenance files. |

## Test Log

- PASS: `go test ./examples/gastown/ -run 'Reaper|Maintenance'`
  - `ok github.com/gastownhall/gascity/examples/gastown 16.363s`
- PASS: `go test ./examples/gastown/`
  - `ok github.com/gastownhall/gascity/examples/gastown 24.921s`
- PASS: `make test-fast-parallel`
  - `All fast jobs passed`
- PASS: `go vet ./...`

## Notes For Mayor

The original remote feature branch still points at reviewed commit `be98b23c3`,
while this release branch carries the same patch rebased on current
`origin/main` as `b19a3db19`. The deploy branch is used to avoid force-pushing
the divergent `work/ga-pftmco-adopt-fix` remote branch.
