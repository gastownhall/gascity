# Release gate: supervisor systemd ownership

- Deploy bead: `ga-8f65f7`
- Review bead: `ga-qtio7p`
- Reviewed commit: `7c750c475d024b77fd713e8d523d8a75875c2191`
- Rebased gated commit: `a23566d7bdc53c16e1c6ed4cd54d778f53bb8add`
- Source branch (provenance only): `builder/ga-9pjtoy`
- Base evaluated: `origin/main@a41b91a032852bc6b6293aa63988d6bdc590c730`
- Merge base: `a41b91a032852bc6b6293aa63988d6bdc590c730`
- Deploy mode: remote; push target: `fork`
- Evaluation date: 2026-09-16
- Verdict: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-qtio7p` is closed with `verdict: pass` for reviewed commit `7c750c475d024b77fd713e8d523d8a75875c2191`. Mayor ruling `gm-wisp-4l5pfx` authorized the ratchet-only rebase described below without another review. |
| 2 | Acceptance criteria met | **PASS** | A directly forked supervisor drops inherited `GC_SESSION_ID` and forces the preserve-sessions-on-signal environment contract. Installed-but-inactive systemd ownership is surfaced before a bare fork, `gc supervisor status` reports ownership, and `gc doctor` checks a live PID against the unit `MainPID`. The proctable regression tests reproduce the unsafe inherited-identity shape and prove the scrubbed child is not selected as an orphan kill target. All ten diff-owned top-level tests pass. |
| 3 | Tests pass | **PASS** | The required full local CI union completed all 40 jobs: **33 job PASS / 7 raw job FAIL**, containing **51,192 top-level PASS / 7 raw FAIL / 227 SKIP**. Every raw failure has the exact shared-server pending-migration refusal signature independently waived by the mayor in `gm-wisp-xm2ylq` and tracked by `ga-lejnse`; no other failure occurred. Each of the ten diff-owned tests passed in both its ordinary and process/integration lane, and a separate focused run passed all ten. The pool-session waiver condition passed ten consecutive runs. Policy, lint/format, build, vet, hook, and boundary checks passed. Details follow. |
| 4 | No high-severity review findings open | **PASS** | Review `ga-qtio7p` records no blocking style, security, specification, or acceptance finding and `uncovered_criteria: none`. The mayor-authorized rebase changes no reviewed non-ratchet content. |
| 5 | Final branch clean | **PASS** | `git status --short` was empty after the test matrix and all additive checks, before this gate record was written. |
| 6 | Branch diverges cleanly from main | **PASS** | After fetching current main, `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `b8de53d94601c2bcbc2fa1751ab0e888f953d007`. The candidate is based directly on current `origin/main`; merge base and base are both `a41b91a032852bc6b6293aa63988d6bdc590c730`. |
| 7 | Single feature theme | **PASS** | The 14-file delta is one supervisor-ownership fix: fork-environment hygiene, systemd ownership observation in status/doctor, its regression tests, and the three required resource-census bookkeeping records. No role behavior or unrelated product change is included. |

## Rebase and review identity

The reviewed two-commit series was rebased from base
`baf96bf442d347f38b997bac31cb0369bf727f9a` onto current main
`a41b91a032852bc6b6293aa63988d6bdc590c730` without conflicts:

- reviewed RED `e0498837c098b3475281003ed91b3453f3fa96b1` maps identically to
  rebased RED `5a6e6fe0900f8f09705b8a44e7003f36b07ef44e`;
- reviewed GREEN `7c750c475d024b77fd713e8d523d8a75875c2191` maps to gated GREEN
  `a23566d7bdc53c16e1c6ed4cd54d778f53bb8add`;
- the old and new non-ratchet stable patch IDs are both
  `2c5d15a143e8f4f6f52411cb13e7e5e098caa1a0`;
- the full patch ID changes from
  `4ab6b89fcfe34a163710700d27f07407e9e304b1` to
  `9842205dab78e26961804e3c864f9ae52547ea09` solely because current main
  already contains the reviewed `666 calls / 193 files` census baseline and
  this candidate adds two subprocess calls in one file;
- mayor ruling `gm-wisp-4l5pfx` independently confirmed that the only rebase
  delta is the resource-census ratchet triple and authorized updating
  `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, and
  `test/test-resources.toml` to `668 calls / 194 files` without re-review.

The resource-census test first failed on the rebased tree with reported
`668/194` against baseline `666/193`, then passed after exactly those three
records were updated. The final range-diff is retained at
`/var/tmp/ga-8f65f7-final-range-diff.log`; RED and GREEN census logs are
`/var/tmp/ga-8f65f7-census-red.log` and
`/var/tmp/ga-8f65f7-census-green.log`.

## Criterion 3 evidence

- `test_cmd`: rootless-Podman invocation of
  `/home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
  with `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`,
  `TESTCONTAINERS_RYUK_DISABLED=true`, `GOFLAGS=-v`,
  `GO_TEST_TIMEOUT=30m`, `LOCAL_TEST_JOBS=4`, and
  `CMD_GC_PROCESS_TOTAL=6`.
- `test_cmd_scope`: full-suite; covers the required real-process `cmd/gc`
  lane as well as unit, integration, formula, tmux-runtime, and REST lanes.
- `test_counts`: 40 jobs total; 33 PASS, 7 raw FAIL; 51,192 top-level PASS,
  7 raw FAIL, 227 SKIP.
- Full log: `/var/tmp/ga-8f65f7-final-full.2UlClO.log`.
- Per-job logs: `/var/tmp/gc-local-tests.cRE7kN/`.
- `diff_tests_executed`: all ten added or modified top-level tests passed
  twice in the full union, with zero diff-owned FAIL or SKIP:
  `TestSupervisorForkEnv`, `TestSupervisorPreForkOwnershipWarning`,
  `TestSupervisorDetermineUnitOwnership`,
  `TestSupervisorUnitOwnershipCheck_Name`,
  `TestSupervisorUnitOwnershipCheck_CanFix`,
  `TestSupervisorUnitOwnershipCheck_WarmupEligible`,
  `TestSupervisorUnitOwnershipCheckRun`,
  `TestBareForkedSupervisorIsSelectedAsOrphanKillTarget`,
  `TestScrubbedForkedSupervisorIsNotSelected`, and
  `TestSetsidDoesNotPreventOrphanSelection`.
- Focused confirmation: the same ten tests plus the resource-census package
  passed at the gated SHA; log `/var/tmp/ga-8f65f7-final-focused.log`.
- Pool-session waiver condition: `go test ./cmd/gc -run
  '^TestCreatePoolSessionBeadWithGuardedAliasSerializesResolvedTmuxAlias$'
  -count=10` passed; log
  `/var/tmp/ga-8f65f7-pool-waiver-count10.log`; tracker `ga-0ujvby`.
- `skip_justification`: all 227 skips are emitted by unchanged suite code for
  platform-specific paths, subprocess helpers, explicit live-provider opt-ins,
  or scenarios assigned to another full-suite lane. None is diff-owned, and
  the real-process `cmd/gc` shards ran.
- `ci_lane_run`: n/a — no CI workflow, matrix, timeout, or required-check
  configuration changed.

### Failure attribution and waiver

All seven raw failures contain the same refusal from `bd init`: bd will not
auto-apply pending schema migrations to a shared server database, reports
that `.beads/metadata.json` was not written, and names explicit coordinated
migration as the remedy. The failures occur during setup before the affected
test body can exercise candidate behavior:

1. `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix`
2. `TestAdoptPRFormulaRetriesTransientReviewerStep`
3. `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries`
4. `TestPersonalWorkFormulaCompileAndRun`
5. `TestGraphWorkflowSuccessPath`
6. `TestCleanInstallTutorialPath`
7. `TestHumaBinary_CityCreateAsync`

Tracker `ga-lejnse` predates this run. Mayor rulings `gm-wisp-bb2nh0` and
`gm-wisp-xm2ylq` authorize attribution by this exact refusal signature, not
by an open-ended test-name list; the latter explicitly inspected the two new
job logs, confirmed the signature, and directed the deploy to proceed at
`a23566d7bd`. In the `TestHumaBinary_CityCreateAsync` shard,
`TestStartDrift_SystemdManaged_RestartsToNewBuildID` passed, confirming the
candidate's real systemd-managed restart path. No different failure message,
real leaked PID, or candidate-owned failure occurred. **Attributed; not
gate-blocking.**

The earlier pool-session flake did not recur in this full run. Its separately
required ten-run focused condition passed as recorded above, satisfying the
narrow extension in the mayor's bead ruling.

### Additional required lanes

- `make test-ci-policy`: PASS
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected fmt-check-changed`: PASS, `0 issues`
- `make build`: PASS
- `go vet ./...`: PASS
- `make check-core-boundary`: PASS
- `make check-native-dependency-surface`: PASS
- `make check-residency-boundary`: PASS, including self-test
- `make check-gomod-replace`: PASS
- `make check-eventexport-isolation`: PASS
- `make check-routed-test-rows check-split-topology-rows`: PASS
- `make check-release-dist-ignore`: PASS
- `make check-hooks`: PASS; `core.hooksPath` is `.githooks`
- `git diff --check`: PASS

## Disposition

All seven release criteria pass. Commit this checklist on the isolated
`deploy/ga-8f65f7-gate` branch, push it to the fork, open the pull request,
publish `release-gate/deploy-clearance` on the exact PR head, and route a
merge request to the mayor. The deployer does not merge.
