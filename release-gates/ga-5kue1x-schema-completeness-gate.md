# Release Gate: ga-5kue1x schema completeness

Bead: ga-5kue1x
Branch: release/ga-5kue1x-schema-completeness-clean
Base: origin/main at 3569d055d1c9392fb2d51bc962205c570c52e1c7
Gate date: 2026-07-06

`docs/PROJECT_MANIFEST.md` / `PROJECT_MANIFEST.md` is not present in this checkout, so no additional project-specific release criteria were available. This gate uses the deployer release criteria from the role instructions.

## Commit Set

| Commit | Source | Summary |
| --- | --- | --- |
| 5774031bc | ga-q7lv2m | test(gc): cover JSON schema declarations |
| 69b0be630 | ga-q7lv2m / ga-mk97qw | fix(schema): add missing JSON result schemas for formula/maintenance commands |
| f1b80ee06 | ga-unvxev / ga-ve0l2c | fix(gc): close JSON schema completeness gaps and fix test env leakage |

The branch was cut cleanly from `origin/main` and does not include the prior failed gate checkpoint commit from `release/ga-5kue1x-schema-completeness-v2`.

## Release Criteria

| # | Criterion | Verdict | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Review bead ga-mk97qw is closed with `REVIEW VERDICT: PASS` for the original schema/runtime JSON changes. Follow-up builder beads ga-unvxev and ga-ve0l2c are closed with verification notes for the test-environment isolation and remaining schema gaps discovered by the prior deploy gate. |
| 2 | Acceptance criteria met | PASS | The branch restores broad schema-file completeness coverage, fixes formula `version-check --json` and maintenance `dolt-gc/status --json` to use `writeCLIJSONLine`, adds result schemas for formula/maintenance/extmsg/perf/runtime JSON outputs, and isolates command-tree tests from ambient city discovery via `clearGCEnv`. |
| 3 | Tests pass | PASS | Focused schema gate: `TMPDIR=/var/tmp/gascity-test-ga-5kue1x go test ./cmd/gc -run 'TestJSONSchemasDeclaredForCommandsWithJSONOutput|TestJSONResultSchemasRequireSuccessDiscriminator|TestActionResultSchemasAllowExtensionFields|TestJSONSchemaManifest' -count=1` passed. Formula/maintenance gate: `TMPDIR=/var/tmp/gascity-test-ga-5kue1x go test ./cmd/gc -run '^(TestFormulaVersionCheck_|TestNewFormulaCmd_RegistersVersionCheckSubcommand|TestRouteMaintenanceStatus_|TestRouteMaintenanceDoltGC_)' -count=1` passed. `TMPDIR=/var/tmp/gascity-test-ga-5kue1x go vet ./...` passed. Broad baseline `make test-fast-parallel` failed only in pre-existing supervisor/managed-Dolt tests; the same focused failing set reproduced on `origin/main` with matching failures (`TestRegisterCityWithSupervisorRejectsStandaloneController*`, `TestSupervisorCreatesControllerSocketForManagedCity`, and the Dolt leak guard), so this branch has zero new test regression versus main. |
| 4 | No high-severity review findings open | PASS | ga-mk97qw notes record a PASS verdict and no blocker/high-severity findings. The prior deploy gate failure was resolved by closed follow-up beads ga-unvxev and ga-ve0l2c. |
| 5 | Final branch is clean | PASS | Clean before writing this gate file; rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base HEAD origin/main` equals `git rev-parse origin/main` (`3569d055d1c9392fb2d51bc962205c570c52e1c7`) after applying the content commits. |
| 7 | Single feature theme | PASS | The commit set touches one subsystem/theme: `gc` CLI JSON result schema completeness and the test isolation needed for that completeness test to be reliable. Touched files are `cmd/gc` JSON/test helpers plus `schemas/**/result.schema.json`. |

## Notes

- Prior gate failure on ga-5kue1x found six additional JSON-capable commands without result schemas. This clean branch includes the follow-up fix and excludes the earlier failed gate artifact from the PR branch.
- The broad fast baseline failure is tracked here as a baseline comparison rather than a branch regression because the same supervisor/managed-Dolt failures reproduce on `origin/main` with the same command and environment.
