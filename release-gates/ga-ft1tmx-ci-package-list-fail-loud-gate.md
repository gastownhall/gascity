# Release gate: CI package-list fail-loud guards

- Deploy bead: `ga-ft1tmx`
- Review bead: `ga-ofxxf2`
- Reviewed commit: `c50338040187bd1922f01059067d17e2ecbcb68e`
- Base evaluated: `origin/main@9700d9a48fb35a2063e1fcb9ee49ab664df26de0`
- Deploy mode: `remote`

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-ofxxf2` is closed with `verdict: pass` for the exact reviewed commit. No review carryover was used. |
| 2 | Acceptance criteria met | PASS | `MAC_UNIT_PKGS` and `UNIT_COVER_PKGS_NONCMDGC` now fail Make evaluation when `go list` returns an empty package set instead of silently invoking `go test` with zero packages. Both compose actions resolve the Go version from `go.mod`, the non-Linux herdr skip guard is present, both affected targets expanded and executed real suites, and the resource-census baselines/documentation match the added tests. |
| 3 | Tests pass | PASS | The documented full local CI aggregate ran through the isolation wrapper. Its four raw failures are tracked and independently proven non-diff-owned as recorded below. All diff-owned tests passed by name. Supplemental runs of the two affected targets also passed. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` passed runner policy, suite coverage, `scripts/cipolicy`, `scripts/prwatchdog`, and static-scope checks. `make build`, `go vet ./...`, and `make check-hooks` also passed. |
| 3c | CI-config lane | PASS | Not applicable: no workflow, job, matrix, timeout, or required-check configuration changed. The diff changes commands used by existing lanes, so their affected Make targets were run directly as supplemental evidence below. |
| 4 | No high-severity review findings open | PASS | Review notes record no style, security, or specification findings; unresolved HIGH findings: 0. |
| 5 | Final branch is clean | PASS | The detached reviewed checkout was clean after all gate commands and generated coverage output was removed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main c50338040187bd1922f01059067d17e2ecbcb68e` exited 0 against the base above. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The Makefile guards, compose-action assertions, platform skip guard, census updates, and testing documentation all support one behavior: required CI lanes must resolve and execute a non-empty package set under the repository's declared Go version. |

## Criterion 3 evidence

### Full CI-equivalent aggregate

- `test_cmd`: `make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- isolation: `isolated-test-run.sh -- make test-local-full-parallel`
- container environment: rootless Podman socket at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`
- result: 40 jobs started; 36 job PASS, 4 job FAIL
- verbose markers: 79,817 PASS, 4 FAIL, 313 SKIP
- `skip_justification`: all 313 markers are declared platform-only, live-service/opt-in, helper-process, build-tag, or fast-unit/process-lane handoff exclusions. No diff-owned test skipped, and the full aggregate included the corresponding process and integration lanes.
- logs: `/var/tmp/ga-ft1tmx-full-gate-20260915/*.log`

### Diff-owned tests

Every test below reported PASS in the full-scope output; several executed in both the unit-core and integration-package sweeps.

- `TestComposeActionsResolveGoVersionFromGoMod`
  - `ubuntu`: PASS
  - `macos`: PASS
- `TestHerdrXDGSocketPathTestsSkipOnNonLinuxPlatforms`: PASS
- `TestMakeDryRunPackageListsAreNonEmpty`
  - `test-mac`: PASS
  - `test-cover-noncmdgc`: PASS
- `TestMakeDryRunFailsLoudlyWhenGoListFails`
  - `test-mac`: PASS
  - `test-cover-noncmdgc`: PASS
- `waiver_ref`: none; no diff-owned test failed or skipped

### Failure attribution

The failing integration tests share no path with this Makefile, documentation, resource-census, and `scripts`-test diff. Both condition trackers predate the run, and today's unrelated `ga-qhr85l` deploy independently reproduced both root conditions.

| Failure | Tracker | Proof and path-overlap result |
|---|---|---|
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `ga-lejnse` | Fresh fixture city refused a random-cursor shared-Dolt migration (`v62 -> v66`). Clause 3(b) CROSS-PR: unrelated `ga-qhr85l` and earlier gates reproduced the same condition. No schema-migration or integration-fixture path overlap. |
| `TestHumaBinary_CityCreateAsync` | `ga-lejnse` | Same tracked concurrent-initializer condition at `v31 -> v66`. Clause 3(b) CROSS-PR; no path overlap. |
| `TestGCLiveContract_BeadsAndEvents` | `ga-lejnse` | Same tracked concurrent-initializer condition at `v28 -> v66`. Clause 3(b) CROSS-PR; no path overlap. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `ga-j88sfp` | Clause 3(b) CROSS-PR: unrelated `ga-qhr85l` reproduced the missing retry today. The tracker also records the test-harness root cause and unmerged fix in PR #6379. No review-formula integration path overlap. |

- `failure_attribution`: mappings above; sightings appended to both trackers during this gate
- clause (i), not diff-owned: satisfied for every failure
- clause (ii), tracked before this run: satisfied for every failure
- clause (iii), independent proof: CROSS-PR evidence above
- clause (iv), no path overlap: satisfied for every failure
- `inconclusive-guard`: not used; every attribution has landed cross-PR proof

### Affected-lane supplemental evidence

- `make test-cover-noncmdgc`: PASS through the isolation wrapper; 25,284 PASS, 0 FAIL, 86 declared SKIP markers; `coverage.noncmdgc.txt` was generated and removed.
- `make test-mac`: PASS through the isolation wrapper on this Linux host, proving `MAC_UNIT_PKGS` expanded into and executed the real non-`cmd/gc` package suite. This is package-expansion evidence, not a claim of Darwin execution; the unchanged Mac workflow remains responsible for the platform run after PR open.
- `make test-ci-policy`: PASS.
- `make build`: PASS.
- `go vet ./...`: PASS.
- `make check-hooks`: PASS.

## Decision

PASS. The full-scope raw failures remain visible and attributed; the changed guards, their failure-mode tests, and both affected CI targets executed successfully.
