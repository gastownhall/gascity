# Release gate: ctime-aware pack content fingerprints

- Deploy bead: `ga-8cce0j`
- Review bead: `ga-kf5zn8`
- Reviewed commit: `84f0af4833b3e59a178b0d4952913fb971f90bbe`
- Base: `origin/main@0b10e4e4d9648cdaf913193b3eed207e71bbdbb9`
- Deploy mode: remote (`gastownhall/gascity`)
- Gate result: **PASS**

The pre-flight lookup found no pull request associated with the reviewed commit,
so the normal release gate applied. Criterion 6 was evaluated first and required
no bounded self-rebase.

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-kf5zn8` is closed with `verdict: pass` on the exact reviewed commit. |
| 2 | Acceptance criteria met | PASS | The fingerprint compares size, mtime, and ctime on Unix; reuses the existing `os.Stat` result; gives the fake filesystem an independent ctime; supplies an explicit Windows approximation; narrows the residual-collision comment; and adds the exact same-size, restored-mtime regression. No wire, schema, OpenAPI, or dashboard surface changes. |
| 3 | Tests pass | PASS | The corrected documented sharded lanes completed 40 PASS / 6 FAIL / 0 SKIP jobs. Five failure signatures satisfy all four pre-existing-failure clauses. `TestCleanInstallTutorialPath` is attributed by the explicit mayor ruling `gm-wisp-feey5w`: its trigger is legacy circuit-breaker cleanup machine state, making a same-state base reproduction unsatisfiable. The diff-owned regression test passed by name with no waiver. |
| 3a | Pre-existing failures attributable | PASS | Four tracked failure classes reproduce on `origin/main` with no path overlap. The tracked tutorial stdout-pollution failure is attributed narrowly to `ga-rsktma` by mayor ruling `gm-wisp-feey5w`, not by base reproduction; the ruling does not waive clause 3 for any other signature. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy`, `make fmt-check-changed`, and `go vet ./...` all exited 0. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no style, security, specification, or uncovered-criteria findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the reviewed commit before this gate record was created. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 84f0af4833b3e59a178b0d4952913fb971f90bbe` produced tree `76eb545010645d8d85cf792346d0b2beb1301c37` with no conflicts. |
| 7 | Single feature theme | PASS | The ancestry-scope guard passed for `ga-8cce0j` and its reviewed source bead `ga-kf5zn8`. All five changed files implement or test one ctime-aware pack-fingerprint behavior under `internal/config` and `internal/fsys`. |

## Test evidence

The gate used checksum-pinned `bd v1.1.0` and Dolt `2.1.7`, an isolated
rootless Podman service, `DOCKER_HOST` pointed at that service, and
`TESTCONTAINERS_RYUK_DISABLED=true`.

- `go test -count=1 -run '^TestPackContentHashRecursiveDetectsMtimePreservingEdit$' -v ./internal/config`
  - 1 PASS / 0 FAIL / 0 SKIP.
  - `diff_tests_executed: TestPackContentHashRecursiveDetectsMtimePreservingEdit PASS`
  - `waiver_ref: none`
- `LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-fast-parallel`
  - 8 PASS / 2 FAIL / 0 SKIP jobs.
- `LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m make test-cmd-gc-process-parallel`
  - 6 PASS / 1 FAIL / 0 SKIP jobs.
- `LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-integration-shards-parallel`
  - 26 PASS / 3 FAIL / 0 SKIP jobs.
- `make test-ci-policy`
  - PASS: 5 runner-policy tests, 15 CI-suite-coverage tests, `scripts/cipolicy`, and the focused static-scope contracts.
- `make fmt-check-changed`
  - PASS.
- `go vet ./...`
  - PASS.

## Failure attribution

The diff changes only:

- `internal/config/fingerprint_ctime_unix.go`
- `internal/config/fingerprint_ctime_windows.go`
- `internal/config/pack.go`
- `internal/config/pack_test.go`
- `internal/fsys/fake.go`

The following failures are not diff-owned, have tracked beads, reproduce on the
base ref, and have no package/path overlap with the diff:

- `TestOSSProjectsNoUnregisteredBackendEnv/source` -> `ga-5em`; base-ref
  reproduction fails when the observed nested-worktree condition is present.
- `TestProviderLiveClaudeKindPath` -> `ga-cqq3hs.1`; base-ref reproduction fails
  with the identical `agent_pane_busy` / `w1:p1` signature.
- `TestGetKeyBinding_CapturesDefaultBinding` and
  `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`; both exact
  base-ref tests fail because the host tmux default key table is empty.
- `TestE2E_SuspendResume_City` -> `ga-rntpsh`; the exact base-ref test fails with
  the same missing `citysus.report` timeout.

The remaining failure has a narrow merge-authority attribution because its
trigger is machine state rather than chance:

- `failure_attribution: TestCleanInstallTutorialPath -> ga-rsktma + mayor ruling
  (state-dependent trigger; clause 3 unsatisfiable, attributed by ruling not by
  base repro)`

Mayor ruling `gm-wisp-feey5w` identifies the defect as a real `bd config get`
stdout-contract violation caused only when legacy closed circuit-breaker files
exist for cleanup. The clean `origin/main` run lacked that machine state, so it
could not reproduce or disprove the defect. The ruling authorizes attribution
for this signature only and explicitly forbids quarantining the test.

The pre-push hook also encountered one teardown-only failure after the test
body passed:

- `failure_attribution: TestCustomTypesCheck_TableDrift -> ga-t33q83 + mayor
  ruling gm-wisp-xnkthw (teardown-only race; test body passed; clause 3 absent,
  attributed by ruling)`

The diff does not own or overlap this test. Its assertions passed, then
`t.TempDir` cleanup reported a lingering eventkit-store lock. The exact
`make test-fast-parallel` lane on
`origin/main@0b10e4e4d9648cdaf913193b3eed207e71bbdbb9` passed all 10 jobs, so a
same-signature base failure was absent. The merge authority authorized this
narrow attribution because the cleanup race carries no information about the
ctime fingerprint change; `ga-t33q83` tracks the underlying fix.
