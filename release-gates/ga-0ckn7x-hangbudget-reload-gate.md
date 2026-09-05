# Release Gate: ga-0ckn7x - reload-test hang budget

Deploy bead: `ga-0ckn7x`  
Review bead: `ga-09qq0u`  
Reviewed content commit: `341069eee3aa90b32afe2ff015600d7f0090acce`  
Evaluated rebased commit: `8061ea62b587668f0e7f58d1777717fe68bb5c54`  
Deploy branch: `deploy/ga-0ckn7x-gate-r2-20260820`  
Base: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`  
Gate evaluated: 2026-08-20  
Verdict: **PASS — MAYOR WAIVER APPLIED**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses
the release criteria in the deployer prompt together with `TESTING.md`,
`engdocs/contributors/release-gate-criteria-conventions.md`, and the shared
non-diff-owned gate-failure protocol.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-09qq0u` records PASS for `341069eee3aa90b32afe2ff015600d7f0090acce`. `git diff --exit-code 341069eee3aa90b32afe2ff015600d7f0090acce 8061ea62b587668f0e7f58d1777717fe68bb5c54 -- cmd/gc/cmd_reload_test.go` returned 0, proving the reviewed reload-test content is byte-identical after rebase. |
| 2 | Acceptance criteria met | PASS | The duplicated initial-reconcile polling loops are replaced by the shared `hangBudget` path. A fresh `go test ./cmd/gc -run Reload -count=1 -v` run passed all executed reload tests; expected real-process cases skipped and were subsequently exercised by the full process union. The synchronized fixed-sleep ledger was re-derived as 451 calls / 164 files for the all-source audit and 300 calls / 116 files for both untagged rows; `TestRepositoryLedgerMatchesCensusAndDocumentation` passed. |
| 3 | Tests pass | **PASS — WAIVED** | `EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel` completed 27 PASS jobs / 13 FAIL jobs. The changed reload tests did not fail. Three failures share the `cmd/gc` package and therefore remain preserved as **FAIL — WAIVED** rather than attributed: `TestCmdStopJSONReportsUnregisteredTrueForSupervisorManagedCity` (`ga-bnjylk`), `TestStopManagedCityForcesCleanupAfterTimeout` (`ga-ifoehb`), and `TestPhase2WorkerCoreRealTransportProof/claude/tmux-cli` (`ga-kgm5nr`). Mayor independently read the candidate and granted a bead- and mechanism-specific waiver because the test-body-only reload diff cannot reach those failures; the resource-census edits are bookkeeping and the removed busy-wait reduces rather than creates load. `waiver_ref: mayor-2026-08-20-ga-0ckn7x-c3` (granted by mayor; audited on `ga-0ckn7x.1` and mail `gm-wisp-fcc2ab`). |
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

## Mayor waiver

Mayor granted `waiver_ref: mayor-2026-08-20-ga-0ckn7x-c3` after independently
reading candidate `8061ea62b5` and the three same-package failures. The ruling
is mechanism-proven rather than tracker-only: the candidate changes reload
test bodies plus mechanically validated resource-census totals, with no shared
state or production path capable of causing the stop or real-transport
failures. Builder then reran the three exact tests successfully under elevated
host load. The residual concurrency reproduction gap is explicitly accepted
by merge authority for this candidate.

The scope is narrow: this bead, this candidate, these three test names, and
their recorded failure mechanisms only. It is not a standing authorization
for other tests, other beads, or different signatures. The raw full-union
result remains 27 PASS / 13 FAIL jobs; the gate does not rewrite it green.

## Disposition

- Remote branch `origin/deploy/ga-0ckn7x-gate-r2-20260820` contains the
  evaluated candidate at `8061ea62b587668f0e7f58d1777717fe68bb5c54`; this
  revised gate record is pushed as the next commit before PR creation.
- Criterion 3 is satisfied only through the explicit mayor waiver above.
- The deployer may open the isolated release PR and route its merge request to
  mayor/mpr. The deployer does not merge.
