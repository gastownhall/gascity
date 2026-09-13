# Release gate: runtime-check nudge-fallback capability smoke test

- Deploy bead: `ga-zzj1pu`
- Build bead: `ga-s5y62b.2`
- Review bead: `ga-o72lfb`
- Reviewed source: `7f97e8afc32d3f1b46f17471b91da27ef4fbcb73`
- Source branch: `builder/ga-s5y62b.2` (provenance only)
- Planned deploy branch: `deploy/ga-zzj1pu-gate`
- Base checked at gate time: `origin/main@e3bee4cd15f279ad11abe9257014e413926b41ba`
- Deploy mode: `remote`; push remote: `origin`
- Gate result: **PASS with one attributed pre-existing test failure**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This record applies
the active seven-criterion deploy gate, the source bead's acceptance criteria,
and the full-suite policy documented in `TESTING.md`.

The already-merged pre-flight found no pull request associated with the exact
reviewed source, so the normal deploy path applies.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-o72lfb` records a PASS verdict for the exact reviewed source. No review carryover is involved. |
| 2 | Acceptance criteria met | **PASS** | Independent source inspection confirms `gc runtime check <name>` reads the selected runtime's declared prompt-delivery strategy and sets `rppcheck.Options.RequireNudge` only for `prompt_delivery = "nudge-fallback"`. The checker performs a real nudge against its throwaway session and hard-fails required unsupported/error cases. Undeclared runtimes retain the prior optional/skip behavior, while handshake, lifecycle, and other capability checks remain intact. All five diff-owned tests passed in the full suite. |
| 3 | Tests pass | **PASS with attributed raw failure** | The documented full-scope 40-job command completed **39 PASS / 1 raw FAIL / 0 skipped jobs**. The only top-level failure was the unchanged installed-`bd` flag-manifest drift tracked by predating bead `ga-f0uceo`; all five diff-owned tests executed and passed. No waiver is used. |
| 3a | Non-diff-owned failures attributed | **PASS** | `TestBdFlagManifestCurrent` is attributed to `ga-f0uceo` under the four-clause mechanism proof below. The tracker predates this run, and the current sighting was appended and read back as comment `292599e5-1f4d-5d11-ad05-de5aa611d39e`. |
| 3b | Policy/lint lane | **PASS** | Repository CI-policy, dependency-surface, event-export, core-boundary, native-DoltLite, docs, corrected fresh-cache affected lint, changed-file formatting, build, and vet checks all passed. Exact commands are recorded below. |
| 3c | CI-config lane run | **PASS / n/a** | No CI job, workflow, matrix, timeout, or required-check list changed. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | The review bead records no style, security, specification, or coverage blocker and no unresolved HIGH finding. |
| 5 | Final branch clean | **PASS** | `git status --porcelain` produced no output at the reviewed source after the full suite and static checks. This checklist is the deployer's sole addition and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after pre-flight. `git merge-tree --write-tree origin/main 7f97e8afc32d3f1b46f17471b91da27ef4fbcb73` exited 0 and produced tree `a87234f71f4eb0536b72b09ca870bcd8bc3d332e`. No bounded self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The two reviewed commits and six changed files implement one runtime-conformance feature: validating a runtime's declared nudge-fallback capability through the existing `gc runtime check` flow. No independent feature is bundled. |

## Source and acceptance evidence

The recorded source was resolved through Git before evaluation:

```text
git rev-parse --verify --quiet '7f97e8afc32d3f1b46f17471b91da27ef4fbcb73^{commit}'
7f97e8afc32d3f1b46f17471b91da27ef4fbcb73
```

The reviewed range contains the TDD red/green pair:

```text
a2cf36be40ed8cefc4dd5e8d0181c0fd2b9f3544 test(rppcheck): red — smoke-test declared nudge-fallback prompt-delivery capability (refs ga-s5y62b.2)
7f97e8afc32d3f1b46f17471b91da27ef4fbcb73 feat(rppcheck): green — smoke-test declared nudge-fallback prompt-delivery capability (refs ga-s5y62b.2)
```

The net diff is limited to:

- `cmd/gc/cmd_runtime_check.go`
- `cmd/gc/cmd_runtime_check_test.go`
- `cmd/gc/cmd_runtime_conformance.go`
- `internal/config/pack_runtimes.go`
- `internal/runtime/rppcheck/rppcheck.go`
- `internal/runtime/rppcheck/rppcheck_test.go`

## Criterion 3 evidence

The test environment was established before the run:

- rootless Podman socket: `unix:///run/user/1000/podman/podman.sock`
- Docker-compatible `_ping`: `OK`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- cached repository-pinned `docker.io/dolthub/dolt:2.1.7` image present
- cached `testcontainers/ryuk:0.14.0` image present

Test record:

- `test_cmd`: `GOFLAGS=-v LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m EXTRA_TEST_ENV="DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true" make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_job_counts`: **39 PASS / 1 raw FAIL / 0 SKIP jobs**
- `test_top_level_counts`: **48,293 PASS / 1 FAIL / 208 SKIP**
- `test_all_level_counts`: **85,344 PASS / 18 FAIL / 299 SKIP**; the additional FAIL entries are the 17 subtests of the one top-level manifest-drift failure
- `full_log`: `/var/tmp/ga-zzj1pu-full-suite.log`
- `shard_logs`: `/var/tmp/gc-local-tests.g5dh30`
- `waiver_ref`: none
- `ci_lane_run`: n/a — no CI-config change

`skip_justification`: all 208 top-level skips were existing suite-controlled
platform, privilege, helper-process, live-provider, or opt-in integration cases;
none was added or modified by this diff. The tutorial case skipped in a short
integration shard also executed and passed in `cmd-gc-process-2-of-6`. No
diff-owned test skipped or failed.

`diff_tests_executed`:

- `TestRuntimeCheckCmd_NudgeFallbackDeclarationIsSmokeTested`: PASS
- `TestRun_RequireNudgeFailsWhenNudgeUnimplemented`: PASS
- `TestRun_RequireNudgeFailsWhenNudgeErrors`: PASS
- `TestRun_RequireNudgePassesWhenNudgeWorks`: PASS
- `TestRun_RequireNudgeStartFailureSkipsWithRequiredName`: PASS

### Raw failure attribution

`failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a), mechanism — attributed`

1. **Not diff-owned:** neither `internal/bdflags` nor its tests are touched by
   the candidate.
2. **Tracked before the run:** open gate tracker `ga-f0uceo`, created
   2026-08-15, names this exact installed-`bd` flag-manifest drift. The current
   sighting was appended after this run.
3. **Not caused by the diff:** the test compares the host-installed `bd` flag
   surface with the static `internal/bdflags` manifest. `go list -deps -test
   ./internal/bdflags` contains none of the candidate's changed packages
   (`cmd/gc`, `internal/config`, or `internal/runtime/rppcheck`). The observed
   missing flags (`--cpu-profile`, `--database`, `--mem-profile`, `--no-color`,
   and command-specific flags) match the tracker's condition.
4. **No path overlap:** the failing package and manifest are outside all six
   changed paths.

Raw failure log:
`/var/tmp/gc-local-tests.g5dh30/integration-packages-core-1-of-4.log`.

## Policy and static evidence

```text
make test-ci-policy                                                    PASS
make check-gomod-replace                                               PASS
make check-native-dependency-surface                                   PASS
make check-eventexport-isolation                                       PASS
make check-core-boundary                                               PASS
make test-native-doltlite-beads                                        PASS
make check-docs                                                        PASS
GOLANGCI_LINT_CACHE=/var/tmp/ga-zzj1pu-lint.1Y6Qjj \
  LINT_CHANGED_SCOPE=tracked \
  LINT_CHANGED_REF=28ddd183f2e2f0b224474a64861ba9a7539284f2 \
  make lint-affected                                                   PASS (0 issues)
LINT_CHANGED_SCOPE=tracked \
  LINT_CHANGED_REF=28ddd183f2e2f0b224474a64861ba9a7539284f2 \
  make fmt-check-changed                                               PASS
go build ./...                                                         PASS
go vet ./...                                                           PASS
```

An earlier diagnostic `lint-affected` invocation used `origin/main` directly
from the detached historical candidate and inherited a stale cross-worktree
cache. That invocation selected main-only history and reported paths from
deleted temporary trees, so it is not scored as candidate evidence. The
recorded fresh-cache merge-base invocation above is the valid policy result.

## Release disposition

**Gate PASS.** Create `deploy/ga-zzj1pu-gate` from the exact reviewed source,
commit this record, push only that isolated branch, open a pull request against
`main`, publish `release-gate/deploy-clearance=success` on the exact PR head,
and route the merge request to mayor/mpr. The deployer does not merge.
