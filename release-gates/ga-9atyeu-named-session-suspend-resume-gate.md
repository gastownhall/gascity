# Release Gate: Preserve named sessions across suspend and resume

Bead: ga-9atyeu  
Implementation bead: ga-pmafyc  
Review bead: ga-4htypx  
Reviewed commit: b046d172e6f32988358b1919c7f42f91b5f942f1  
Reviewed merge-base: 1b991d75c3a2464296f939acd3dd712a25b0d89d  
Gate base: origin/main@63c2ad893a36708ebfb089c755bf45adf467e1ce  
Gate date: 2026-09-12

`docs/PROJECT_MANIFEST.md` is not present in this repository. This checklist
uses the deployer release criteria, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Gate results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-4htypx is closed with `verdict: pass` on the exact reviewed commit. The SHA resolves locally to b046d172e6f32988358b1919c7f42f91b5f942f1. |
| 2 | Acceptance criteria met | PASS | Configured named sessions retain their identity through suspend, drain acknowledgement, and transient start rollback; genuine startup deaths still roll back with or without a session key. All seven diff-owned regression tests passed twice in the full suite. `TestE2E_SuspendResume_City` passed in the full suite and in two additional independent runs (34.52s, 36.59s, 36.57s). `TestGastown_Reconciler_SessionRestartsAfterExit` passed in the full suite and five additional candidate processes, each observing 2-3 restarts. |
| 3 | Tests pass | PASS | The documented full local CI sweep ran all 40 job logs: 50,003 PASS, 2 FAIL, 210 SKIP. Both failures occurred during fixture `gc init` on the shared-server pending-schema-migration refusal before either test's scenario could run; they are non-diff-owned and attributed to pre-existing tracker ga-esyijp under all four attribution proofs below. All required `cmd_gc_process` shards passed, all diff-owned tests executed, and no diff-owned test failed or skipped. |
| 4 | No high-severity review findings open | PASS | Review bead ga-4htypx records no style, security, specification, or other HIGH findings. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit before this gate file was created. The gate file is the only deploy-only addition and will be committed as the isolated branch tip. |
| 6 | Branch diverges cleanly from main | PASS | After refreshing `origin/main`, `git merge-tree --write-tree origin/main b046d172e6f32988358b1919c7f42f91b5f942f1` succeeded and produced tree 623ee83da4d484f3b0d2ce396ba8ab1ad98f51c9. No self-rebase was required. The deploy ancestry scope guard also passed for ga-9atyeu/ga-pmafyc/ga-wwnbvk. |
| 7 | Single feature theme | PASS | The ten TDD commits change one subsystem and one behavior: the session reconciler's handling of configured named-session identity around suspend/resume and startup rollback. |

## Acceptance evidence

- `TestDiscoverSessionBeadsBackfillsConfiguredNamedIdentityOutsideDesiredState`:
  PASS twice.
- `TestReconcileSessionBeads_DrainAckPreservesConfiguredNamedSession`: PASS
  twice.
- `TestReconcileSessionBeads_SuspendedNamedSessionInsideDesiredStateDrainsAsSuspended`:
  PASS twice.
- `TestReconcileSessionBeads_PoolFreeableIgnoresNamedAlwaysOutsideDesiredState`:
  PASS twice.
- `TestReconcileSessionBeads_RollsBackPendingCreatePreservesConfiguredNamedSession`:
  PASS twice.
- `TestReconcileSessionBeads_RollsBackConfiguredNamedSessionThatDiedDuringStartup`:
  PASS twice.
- `TestReconcileSessionBeads_RollsBackSessionThatDiedDuringStartupWithoutSessionKey`:
  PASS twice.
- `TestE2E_SuspendResume_City`: PASS three deployer-controlled runs (34.52s,
  36.59s, 36.57s).
- `TestGastown_Reconciler_SessionRestartsAfterExit`: PASS in the full sweep
  (2.04s, four restarts), then PASS in five independent candidate processes
  (2.06s-2.33s, two or three restarts each). The reviewed merge-base also
  passed five independent processes (1.80s-8.59s, two or three restarts),
  confirming the narrowed startup-death signal preserves the existing prompt
  normal-exit restart behavior.

## Test evidence integrity

- `test_cmd_scope: full-suite`
- `test_cmd:`

  ```text
  DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
  TESTCONTAINERS_RYUK_DISABLED=true \
  EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
  GOFLAGS=-v LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m \
  LOCAL_TEST_LOG_DIR=/var/tmp/ga-9atyeu-full.g22FdB \
  make test-local-full-parallel
  ```

- `test_counts: 50003 PASS, 2 FAIL, 210 SKIP` across 40 job logs. Thirty-eight
  job logs were green; the two red logs contain only the attributed failures
  below.
- `diff_tests_executed:` all seven tests listed in Acceptance evidence ran in
  both the `cmd-gc-process` and `integration-packages-cmd-gc` required lanes;
  14 PASS observations, 0 FAIL, 0 SKIP.
- `skip_justification:` 210 suite-declared platform, optional/live,
  persistence, or helper skips; none is added or modified by this diff.
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`
- `policy_lane: make test-ci-policy — PASS`
- The `cmd_gc_process` path filter applies because this diff changes
  `cmd/gc/**`. All six configured process shards completed successfully in
  the full sweep; this includes the coverage that the default fast tier omits.

## Pre-existing failure attribution

`failure_attribution:`

- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-esyijp`
  (mechanism proof): fixture `gc init` refused 32 pending schema migrations on
  the shared server (v34 to v66), before the managed-worker recovery scenario
  began.
- `TestHumaBinary_CityCreateAsync -> ga-esyijp` (mechanism proof): city
  creation failed while fixture `gc init` refused 30 pending schema migrations
  on the shared server (v36 to v66), before the API scenario could run.

All four attribution requirements hold for both sightings:

1. Neither failure is diff-owned. Their test files are
   `test/integration/review_formula_test.go` and
   `test/integration/huma_binary_test.go`; the candidate changes only five
   `cmd/gc` reconciler files.
2. Open `gate-tracker` bead ga-esyijp covers this shared-server schema
   condition and predates this run (created 2026-08-29). This run's sightings
   were appended to that tracker.
3. The landed mechanism proof is the exact fixture-init refusal above. It
   occurs before either failing test can reach candidate behavior.
4. There is no path overlap between either failing test and the candidate
   diff.

## Additional static gates

```text
make test-ci-policy
go build ./...
go vet ./...
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=1b991d75c3a2464296f939acd3dd712a25b0d89d make fmt-check-changed
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=1b991d75c3a2464296f939acd3dd712a25b0d89d make lint-changed
make lint-new LINT_BASE=1b991d75c3a2464296f939acd3dd712a25b0d89d
git diff --check origin/main...HEAD
```

All commands passed. `git config core.hooksPath` reports `.githooks`.
