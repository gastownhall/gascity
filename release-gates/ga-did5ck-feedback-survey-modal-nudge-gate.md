# Release gate: feedback-survey modal nudge delivery (`ga-did5ck`)

Gate result: **FAIL**

- Evaluated: 2026-09-01
- Deploy mode: `remote`
- Base: `origin/main@3d317457dd7a7ff68f1c0333f7eb5fb399b2016c`
- Reviewed source: `59eefdd9e89021fff799dc8716a3fd60f335ee01`
- Source provenance branch: `builder/ga-zg7fjq` (not mutated or used as a deploy push target)
- Merge base: `bc1c3ccaf774675acb9cc8955093ab8221946daf`
- Review bead: `ga-t7geya`
- Gate-evidence branch: `deploy/ga-did5ck-gate-fail` (no pull request)

`docs/PROJECT_MANIFEST.md` is absent at the reviewed source, so this checklist
uses the seven criteria from the active deployer protocol together with
`TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Preflight

The mandatory already-merged preflight found no pull request associated with
the reviewed source commit in `gastownhall/gascity`. The gate therefore
proceeded to criterion 6 before evaluating any expensive test lane.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Reviewer PASS present | SKIPPED | Fail-fast after criterion 6. As input only, `ga-t7geya` is closed with reason `pass` and records the exact reviewed source SHA. |
| 2 | Acceptance criteria met | SKIPPED | Fail-fast after criterion 6; acceptance was not re-evaluated against a source that cannot merge cleanly into current `main`. |
| 3 | Tests pass | SKIPPED | The deployer protocol forbids running the full test lanes after an unresolved criterion-6 failure. `test_cmd_scope: not run`; `test_counts: 0 PASS, 0 FAIL, 0 SKIP`; `diff_tests_executed: not run (criterion-6 fail-fast)`; `waiver_ref: none`; `ci_lane_run: n/a (no CI-config paths in the diff)`. |
| 3b | Required policy/lint lanes | SKIPPED | Fail-fast after criterion 6; no policy lane result is asserted for this gate attempt. |
| 4 | No high-severity review findings open | SKIPPED | Fail-fast after criterion 6. The reviewer recorded no blocking finding, but this gate does not rely on that input after the freshness failure. |
| 5 | Final branch is clean | SKIPPED | Fail-fast after criterion 6. The isolated rebase clone was clean before the attempt, and the canonical helper restored it cleanly at the reviewed SHA afterward. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree origin/main 59eefdd9e89021fff799dc8716a3fd60f335ee01` reported content conflicts in `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, `scripts/runtime_tmux_manifest_test.go`, and `test/test-resources.toml`. The required `attempt_bounded_self_rebase builder/ga-zg7fjq main` with `PUSH_REMOTE=origin` returned `12`, classified the conflicts as non-trivial, aborted, and restored `HEAD` from `59eefdd9e89021fff799dc8716a3fd60f335ee01` to the identical SHA. |
| 7 | Single feature theme | SKIPPED | Fail-fast after criterion 6. |

## Disposition

This is a technical gate failure. The builder must rebase the feedback-survey
modal nudge fix onto current `origin/main`, reconcile the four conflicting
test-policy/documentation files, rerun the required verification, and return a
new exact SHA for review. The provenance branch was not pushed or modified; no
pull request was opened and no deploy-clearance status was created.
