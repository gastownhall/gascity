# Release Gate: ga-0ckn7x - reload-test hang budget

Deploy bead: `ga-0ckn7x`  
Review bead: `ga-09qq0u`  
Reviewed content commit: `341069eee3aa90b32afe2ff015600d7f0090acce`  
Evaluated rebased commit: `8061ea62b587668f0e7f58d1777717fe68bb5c54`  
Deploy branch: `deploy/ga-0ckn7x-gate-r2-20260820`  
Base: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`  
Gate evaluated: 2026-08-20  
Verdict: **FAIL**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses
the release criteria in the deployer prompt together with `TESTING.md`,
`engdocs/contributors/release-gate-criteria-conventions.md`, and the shared
non-diff-owned gate-failure protocol.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-09qq0u` records PASS for `341069eee3aa90b32afe2ff015600d7f0090acce`. `git diff --exit-code 341069eee3aa90b32afe2ff015600d7f0090acce 8061ea62b587668f0e7f58d1777717fe68bb5c54 -- cmd/gc/cmd_reload_test.go` returned 0, proving the reviewed reload-test content is byte-identical after rebase. |
| 2 | Acceptance criteria met | PASS | The duplicated initial-reconcile polling loops are replaced by the shared `hangBudget` path. A fresh `go test ./cmd/gc -run Reload -count=1 -v` run passed all executed reload tests; expected real-process cases skipped and were subsequently exercised by the full process union. The synchronized fixed-sleep ledger was re-derived as 451 calls / 164 files for the all-source audit and 300 calls / 116 files for both untagged rows; `TestRepositoryLedgerMatchesCensusAndDocumentation` passed. |
| 3 | Tests pass | **FAIL** | `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel` completed 27 PASS jobs / 13 FAIL jobs. The changed reload tests did not fail. However, three failing tests are in the same `cmd/gc` package as the diff: `TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity` (`ga-bnjylk`), `TestStopManagedCityForcesCleanupAfterTimeout` (`ga-ifoehb`), and `TestPhase2WorkerCoreRealTransportProof/claude/tmux-cli` (`ga-kgm5nr`). They are not diff-owned, but attribution clause 4 requires no package-path overlap; that clause is unsatisfied, so criterion 3 fails. `waiver_ref: none`. |
| 4 | No high-severity review findings open | PASS | The reviewer verdict is PASS with no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on the rebased deploy branch before this gate record was written. The only manual rebase edits are the mechanically synchronized resource-census totals validated by the live census test. |
| 6 | Branch diverges cleanly from main | PASS | The reviewed commit was manually replayed onto current `origin/main`. Only the known three-file resource-census ledger conflict required resolution; the implementation file applied cleanly. `origin/main` is an ancestor of `8061ea62b587668f0e7f58d1777717fe68bb5c54`, and the branch was pushed successfully after all 10 fast pre-push jobs passed. |
| 7 | Single feature theme | PASS | One commit changes `cmd/gc/cmd_reload_test.go` and its synchronized fixed-sleep policy ledger only. No unrelated feature is bundled. |

## Criterion 3 evidence

Full-union logs: `/var/tmp/gc-local-tests.yJ2Lpa`.

Same-package failures that block attribution under clause 4:

- `TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity` timed out
  after 1 second (`ga-bnjylk`).
- `TestStopManagedCityForcesCleanupAfterTimeout` took 4.640 seconds and missed
  its asserted bound (`ga-ifoehb`, filed from this run).
- `TestPhase2WorkerCoreRealTransportProof/claude/tmux-cli` returned
  `context deadline exceeded` during worker start (`ga-kgm5nr`, filed from
  this run).

The remaining failures have no path overlap with the candidate and match
existing tracked signatures:

- `TestSQLiteWriterFenceSIGKILLAtReservationBoundaries` -> `ga-ggrykt`.
- `TestBdFlagManifestCurrent` -> `ga-f0uceo`.
- `TestGetKeyBinding_CapturesDefaultBinding` -> `ga-afqddr`.
- `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-k3fxvj`.
- Five review-formula dirty-table schema migration failures -> `ga-lpfjhc`
  (gastownhall/beads#4566).
- `TestDoltConfigWiringExternalHost` -> `ga-gajll3`.
- `TestCleanInstallTutorialPath` stdout pollution -> `ga-hrdd3h`.

The host was under exceptional concurrent load during the run (the 1-minute
load average rose from roughly 53 to 86), but load is context rather than a
waiver. No retry was used to erase the failures.

## Disposition

- Remote branch `origin/deploy/ga-0ckn7x-gate-r2-20260820` contains the
  evaluated candidate at `8061ea62b587668f0e7f58d1777717fe68bb5c54`.
- No PR, clearance status, or merge was created.
- Return to builder because criterion 3 attribution clause 4 is unsatisfied
  for the three same-package failures. A future deploy evaluation needs a
  clean required result or an explicit merge-authority waiver; the deployer
  cannot grant that waiver.
