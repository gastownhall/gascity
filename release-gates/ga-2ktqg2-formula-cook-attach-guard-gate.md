# Release Gate: Formula Cook Attach Guard and Self-Rooting

- Deploy bead: `ga-2ktqg2`
- Build bead: `ga-qiplt2`
- Originally reviewed commit: `b27bae0ae9e8d9111bf726ab9a4fc30e52eca2f2`
- Evaluated re-gate commit: `4ef3bfd26f643a4b0d0beaf8eb22616211f8e42b`
- Base: `origin/main@8c73625b974ce8d3d68d54ab42062e6247c47036`
- Deploy mode: remote
- Deploy branch: `deploy/ga-2ktqg2-gate`
- Verdict: **FAIL — WAIVED BY MAYOR**

The evaluated commit is a mechanical rebase of the reviewed change. A direct
four-file comparison between the original reviewed commit and the evaluated
commit is empty, so the reviewer-approved content did not drift. The associated
PR preflight found no pull request for the evaluated commit. Criterion 6 was
evaluated first and again after the long test run.

The repository does not contain `docs/PROJECT_MANIFEST.md`; this record applies
the seven criteria in the active deployer contract and the evidence convention
in `engdocs/contributors/release-gate-criteria-conventions.md`.

The technical test gate remains a failure. The Mayor's 2026-08-18 standing
authorization in `ga-vb82ss`, section 3, permits deployment when every failing
test has a specific tracker, the diff has no plausible mechanism to reach the
failure, each occurrence is logged on its tracker, and the gate preserves the
failure as waived rather than rewriting it green. All four conditions are met
below; no failure is in a changed file or the formula-attachment/molecule-rooting
subsystem.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-2ktqg2` records an independent reviewer PASS for `b27bae0ae9e8d9111bf726ab9a4fc30e52eca2f2`. The four reviewed files are byte-identical at the evaluated re-gate commit `4ef3bfd26f643a4b0d0beaf8eb22616211f8e42b`. |
| 2 | Acceptance criteria met | **PASS** | The v1 `gc formula cook --attach` path now calls the existing `sling.CheckNoMoleculeChildren` guard before mutation, and the regression test proves a live existing attachment is refused without creating beads. `molecule.AttachOptions.SelfRoot` is opt-in and roots this caller's sub-DAG at the explicit attach target; the new test proves sibling molecule metadata cannot mis-root it, while the pre-existing run-chain test proves default chain-walking remains intact. Build, vet, policy, lint, and boundary checks pass. |
| 3 | Tests pass | **FAIL — WAIVED** | `make test-local-full-parallel` ran all required fast, process-backed cmd/gc, and integration lanes: **25 PASS jobs, 15 FAIL jobs, 0 SKIP jobs**. Both diff-owned tests PASS by name with 0 FAIL and 0 SKIP. Every failure is non-diff-owned, specifically tracked, logged on that tracker, and structurally unreachable from the changed formula/molecule path; see the attribution table. `waiver_ref`: Mayor's 2026-08-18 standing authorization recorded in `ga-vb82ss`, section 3. |
| 4 | No high-severity review findings open | **PASS** | The review records no blocking security, correctness, or style finding. Unresolved HIGH findings: `0`. |
| 5 | Final branch is clean | **PASS** | Before this checklist was created, `git status --porcelain=v1` was empty at the evaluated commit. `git diff --check origin/main...HEAD` produced no output. `git config core.hooksPath` reports `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | No associated PR exists. After a fresh `git fetch origin main`, `git merge-base --is-ancestor origin/main 4ef3bfd26f643a4b0d0beaf8eb22616211f8e42b` succeeds; the merge base is exactly `8c73625b974ce8d3d68d54ab42062e6247c47036`. Rechecked after the full test union; no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One commit changes four files in one feature: prevent duplicate v1 formula attachments and keep their sub-DAG rooted at the caller-named target. No independent behavior is bundled. |

## Test evidence

Environment established before criterion 3:

- Rootless Podman socket active at `unix:///run/user/1000/podman/podman.sock`.
- `TESTCONTAINERS_RYUK_DISABLED=true`.
- Cached `docker.io/dolthub/dolt-sql-server:2.1.7` matches `deps.env`'s
  `DOLT_VERSION=2.1.7` pin.
- No `dolt-tests-via-podman` cairn entry exists for this rig.

Required and supporting commands:

- `make test-local-full-parallel` with `DOCKER_HOST` and
  `TESTCONTAINERS_RYUK_DISABLED` passed through `EXTRA_TEST_ENV`: **25 PASS
  jobs, 15 FAIL jobs, 0 SKIP jobs** across the runner's 40 named jobs. Logs:
  `/var/tmp/gc-local-tests.DGBY77`.
- `go test -count=1 -v ./cmd/gc -run '^TestFormulaCookAttachRefusesWhenTargetAlreadyHasMoleculeAttached$'`:
  **1 PASS, 0 FAIL, 0 SKIP**.
- `go test -count=1 -v ./internal/molecule -run '^TestAttachSelfRootIgnoresSiblingMoleculeAttachment$'`:
  **1 PASS, 0 FAIL, 0 SKIP**.
- `go test -count=1 -v ./internal/molecule -run '^TestAttachResolvesRootFromRunChainNotOwnID$'`:
  **1 PASS, 0 FAIL, 0 SKIP**; verifies the opt-in did not change the default
  run-chain contract.
- `make test-ci-policy`: PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected`:
  PASS, 0 issues.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed`:
  PASS.
- `make check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads`:
  PASS.
- `make vet`: PASS.
- `go build ./...`: PASS.

`diff_tests_executed`:

- `TestFormulaCookAttachRefusesWhenTargetAlreadyHasMoleculeAttached`: PASS.
- `TestAttachSelfRootIgnoresSiblingMoleculeAttachment`: PASS.

`test_counts`: required full union `25 PASS jobs / 15 FAIL jobs / 0 SKIP
jobs`; diff-owned tests `2 PASS / 0 FAIL / 0 SKIP`.

`skip_justification`: not applicable — the runner reported zero skipped jobs,
and neither diff-owned test skipped.

`waiver_ref`: Mayor's 2026-08-18 ruling in `ga-vb82ss`, section 3 (broadened
standing authorization for fully tracked failures outside the diff's reachable
files/subsystem). The gate remains **FAIL — WAIVED**.

## Failure attribution

| Failure(s) | Tracker | Structural mechanism proof |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | Reads the installed `bd --help` surface from `internal/bdflags`; the candidate changes neither that package nor the external binary. Occurrence logged on the tracker. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-afqddr` | Depend on host tmux 3.7b's default keytable in `internal/runtime/tmux`; formula cook and molecule attachment do not reach it. Occurrences logged. |
| `TestBdRigWorktreeStoreConsistentAcrossRawBdGcBdAndProviderStore` | `ga-227zz7` | Stops in fixture setup at `beads.OpenNativeStorage`'s hard 15-second schema-open context, before the test reaches its assertions. It never invokes `gc formula cook`; the new tracker was created and read back in this gate session. |
| `TestAdoptPRFormulaCompileAndRun`, `TestAdoptPRFormulaRetriesTransientReviewerStep`, `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`, `TestPersonalWorkFormulaCompileAndRun`, `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash`, `TestGraphWorkflowSuccessPath`, `TestGraphWorkflowFailureRunsCleanup`, `TestHumaBinary_CityCreateAsync` | `ga-lpfjhc` | All eight stop during throwaway-city `gc init` with the exact `gastownhall/beads#4566` dirty-table migration error, before any formula execution or molecule attachment. Occurrences logged together on the tracker. |
| `TestCustomTypesCheck_TableDrift` | `ga-6pnurv` | Fails only in `testing.TempDir` cleanup in `internal/doctor`; the candidate cannot reach that package's cleanup writer. Occurrence logged; its reviewed budget fix remains pending in deploy bead `ga-t33q83`. |
| `TestSQLiteLegacySnapshotSIGKILLAtBoundaries/legacy-claim-release-after` | `ga-5dsf6n` | Times out in the standalone `internal/storebinding/sqlite` child protocol under parallel load; no formula/molecule code participates. Occurrence logged. |
| `TestDoltConfigWiringExternalHost` | `ga-gajll3` | Exceeds the integration helper's hard `bd init` timeout after initialization succeeds, before the candidate path is invoked. Occurrence logged; the tracker records the unmerged timeout fix. |
| `TestCleanInstallTutorialPath` | `ga-hrdd3h` | Fails because circuit-breaker diagnostics contaminate `bd config get issue_prefix` output; formula attachment and molecule rooting cannot affect that subprocess stdout. Occurrence logged. |

## Waiver disposition

Criterion 3 remains **FAIL**. The 15 red jobs contain only the tracked
signatures above. None failed in `cmd/gc/cmd_formula.go`,
`cmd/gc/cmd_formula_test.go`, `internal/molecule/molecule.go`,
`internal/molecule/attach_test.go`, or the attachment/rooting behavior they
exercise. The one same-package cmd/gc failure stopped in native fixture-store
setup and never called the changed command path. Both diff-owned regressions
passed independently by name.

Under the standing Mayor authorization recorded in `ga-vb82ss`, this retained
failure is waived for deployment. Cut and push only the isolated deploy branch,
open the pull request, publish deploy clearance on its exact gated head, and
route merge authority to mayor/mpr. The deployer does not merge.
