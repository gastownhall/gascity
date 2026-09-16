**Verdict:** **FAIL**

# Release Gate: ga-wz3iip - named-session alias resolution

Deploy bead: `ga-wz3iip`
Review bead: `ga-8hoz1q`
Build bead: `ga-e3o1dq`
Reviewed commit: `983eb81bb212ea60ffa366d470c6c61473b89b05`
Base checked: `origin/main@fae7c69647497d377c4ad89478545e296ac92f97`
Gate evaluated: 2026-09-16

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 6 | Branch diverges cleanly from main | **FAIL** | The reviewed source conflicts with current main in `internal/session/named_config_test.go`; `git merge-tree --write-tree origin/main 983eb81bb212ea60ffa366d470c6c61473b89b05` exited nonzero. The mandated bounded self-rebase was attempted on the internally authored source branch after resolving `PUSH_REMOTE=origin`. It produced a clean local rebased tip `c849a1564d75a225e6ff427e0fa839bf20c59440` (merge tree `597b4c42ddc402f0d3ab381458befc836d653b9d`) but its lease-guarded push returned `rc=13`. The remote branch remained exactly at reviewed SHA `983eb81bb212ea60ffa366d470c6c61473b89b05`, confirmed by both the tracking ref and `git ls-remote`. Per the bounded-self-rebase contract, any `rc=13` is a criterion-6 failure and routes back to the builder; the deployer may not retry or bypass it. |
| 1 | Review PASS present | **SKIPPED** | Fail-fast after criterion 6. Review bead `ga-8hoz1q` does contain a PASS for the reviewed source, but the rebased local tip was not published and is not deployable. |
| 2 | Acceptance criteria met | **SKIPPED** | Fail-fast after criterion 6. |
| 3 | Tests pass | **SKIPPED** | Fail-fast after criterion 6; the protocol forbids spending the full-suite run after an unsuccessful bounded self-rebase. |
| 4 | No high-severity review findings open | **SKIPPED** | Fail-fast after criterion 6. |
| 5 | Final branch is clean | **SKIPPED** | Fail-fast after criterion 6. |
| 7 | Single feature theme | **SKIPPED** | Fail-fast after criterion 6. |

## Pre-flight and disposition

Original PR #5443 is still open and unmerged, so the already-merged pre-flight
did not reconcile this bead. Its old head remains conflicting against main.

No criterion-3 run, isolated deploy branch, push, pull request, or deploy
clearance was created. Route the bead back to the builder with the exact
`rc=13` evidence. Any replacement PR must still name `ga-jhi49l` alongside
`ga-t3a0fv`, as required by the review handoff.
