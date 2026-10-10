# Release gate: bounded supervisor stop

- Deploy bead: `ga-hjkv6q`
- Build beads: `ga-0plp2i`, `ga-y0reyu`
- Review bead: `ga-iyp1jk`
- Reviewed source: `8796af0e97e8258b75958387288e29921f49de0f`
- Base checked: `origin/main@175059246d40a5223748e01ee5c5772727772a74`
- Target issue: `gastownhall/gascity#5256`
- Decision: **PASS**

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-iyp1jk` is closed with an explicit round-4 PASS on the exact reviewed source. The reviewer recorded zero high-severity findings. |
| 2 | Acceptance criteria met | PASS | Both ordinary and preserve-sessions supervisor shutdown now bound a blocking city-runtime shutdown with the configured forced-stop deadline. The same deadline covers the shutdown call and the subsequent wait, so the budget cannot be consumed twice. A never-exiting city still returns a non-nil error, while the normal clean path is unchanged. `TestStopManagedCityBoundsForcedShutdownWhenRuntimeHangs` independently exercised the combined grace-plus-forced ceiling in both full-suite cmd/gc lanes and passed. |
| 3 | Tests pass | PASS | The documented full-scope 40-job suite ran twice on the exact reviewed source, with the second run verbose for exact test counts. The diff-owned hanging-runtime regression passed in both relevant shards in both runs. All raw failures are non-diff-owned, have exact pre-existing trackers, have mechanism or reachability proof, and have no path overlap; attribution is recorded below. |
| 4 | No high-severity review findings open | PASS | The reviewer recorded no HIGH findings, no security blocker, and no unresolved spec finding. |
| 5 | Final branch is clean | PASS | The reviewed source was clean through the full-suite and static lanes; this checklist is the sole release-evidence addition. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 8796af0e97e8258b75958387288e29921f49de0f` succeeded at the refreshed base above and produced tree `804a29195cc22bf6be657d504a1874f4fa83637a`; no self-rebase was required. |
| 7 | Single feature theme | PASS | Four commits and two `cmd/gc` files implement and test one supervisor-shutdown theme. The ancestry is deliberately scoped to the deploy bead and its two same-feature build beads; no unrelated or `.claude/**` path is present. |

## Test evidence

`test_cmd_scope: full-suite`

The mandatory fresh full-suite run used the repository's documented local CI-equivalent target:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
LOCAL_TEST_LOG_DIR=/var/tmp/gascity-ga-hjkv6q.rOay7B \
make test-local-full-parallel
```

- `job_counts: 33 PASS, 7 attributed FAIL, 0 SKIP out of 40 jobs`
- `initial_raw_failures: 9 top-level FAIL, all attributed below`

After the host load returned to the tracked safe start band, a second unchanged full-scope run enabled verbose Go output so the gate could record exact per-test counts:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
GOFLAGS=-v LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GO_TEST_TIMEOUT=30m \
LOCAL_TEST_LOG_DIR=/var/tmp/gascity-ga-hjkv6q-verbose.Os6CAJ \
make test-local-full-parallel
```

- `test_counts: 45662 PASS, 8 attributed FAIL, 200 SKIP`
- `diff_tests_executed: TestStopManagedCityBoundsForcedShutdownWhenRuntimeHangs PASS` in `cmd-gc-process-1-of-6` and `integration-packages-cmd-gc-3-of-6` in both full-suite runs
- `skip_justification: the 200 SKIPs are pre-existing platform, helper-process, optional-provider, live-service, or explicitly gated real-infrastructure cases. The diff-owned regression did not skip and passed twice in each of its two full-suite lanes.`
- `waiver_ref: none`
- `ci_lane_run: n/a (no CI-config change)`

### Failure attribution

| Raw result | Tracker | Attribution |
|---|---|---|
| FAIL: `TestCatalogMatchesProductionWiringAndDocumentation` twice per run | `ga-cojd80` | Clause 3(a), mechanism plus reachability: the reviewed checkout's provider-ledger waiver rows expired before these runs. The provider-ledger package cannot import or execute `cmd/gc`, and paths do not overlap. |
| FAIL: `TestBdFlagManifestCurrent` once per run | `ga-f0uceo` | Clause 3(a), mechanism plus reachability: the installed `bd` flag surface has drifted from this older reviewed checkout's manifest. The manifest package cannot import or execute `cmd/gc`, and paths do not overlap. |
| FAIL: `TestProviderLiveClaudeKindPath` twice | `ga-iepsvr` | Clause 3(a), mechanism plus reachability: the herdr probe reported the exact tracked `agent_pane_busy` host condition. The herdr package cannot import or execute `cmd/gc`, and paths do not overlap. |
| FAIL: `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` once per run | `ga-k3fxvj` | Clause 3(a), mechanism plus reachability: the host tmux returned the tracked empty filtered default bindings. Runtime/tmux cannot import or execute `cmd/gc`, and paths do not overlap. |
| FAIL: `TestGraphWorkflowSuccessPath` and `TestCleanInstallTutorialPath` in the initial run | `ga-vkhfnj` | Clause 3(a), mechanism: both failed during `gc init`, before the changed supervisor-stop path could execute, because concurrent use left shared Dolt tables dirty. The diff does not touch the integration tests or the init path. |
| FAIL: `TestAdoptPRFormulaRetriesTransientReviewerStep` and `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` in the verbose run | `ga-vkhfnj` | Clause 3(a), mechanism: both failed during `gc init` on the tracked shared-Dolt dirty-table migration refusal, before formula execution or supervisor shutdown. Paths do not overlap. |
| FAIL: `TestCleanInstallTutorialPath` in the verbose run | `ga-vkhfnj` | Clause 3(a), mechanism: a shared circuit-breaker cleanup message contaminated `bd config` stdout before any supervisor-stop path ran. The candidate does not touch config output, circuit-breaker handling, or this test. |

`failure_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | clause 3(a) mechanism/reachability — expired provider-ledger waiver rows; changed cmd/gc path unreachable`

`failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism/reachability — installed-bd manifest drift; changed cmd/gc path unreachable`

`failure_attribution: TestProviderLiveClaudeKindPath -> ga-iepsvr | clause 3(a) mechanism/reachability — tracked herdr pane contention; changed cmd/gc path unreachable`

`failure_attribution: TestGetKeyBinding_CapturesDefaultBinding{,WithArgs} -> ga-k3fxvj | clause 3(a) mechanism/reachability — tracked host tmux keytable behavior; changed cmd/gc path unreachable`

`failure_attribution: TestGraphWorkflowSuccessPath,TestCleanInstallTutorialPath,TestAdoptPRFormulaRetriesTransientReviewerStep,TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-vkhfnj | clause 3(a) mechanism — shared-Dolt setup/output condition occurs before the changed supervisor-stop path`

No failure is diff-owned, no failing-test path overlaps the diff, and the change adds no test target or resource-census load. Sightings from both runs were appended to all five trackers and read back from the bead store.

## Required lanes

- `policy_lane: make test-ci-policy — PASS`
- `go vet ./...` — PASS
- `make lint-changed LINT_CHANGED_REF=$(git merge-base origin/main HEAD)` with a fresh `/var/tmp` analyzer cache — PASS (`0 issues` in `./cmd/gc`)
- `make fmt-check-changed LINT_CHANGED_REF=origin/main` — PASS
- `git diff --check origin/main...HEAD` — PASS

One supplemental full-repository lint attempt was discarded as invalid evidence: a shared analyzer cache returned 54 diagnostics rooted in a missing, unrelated `/var/tmp/ga-xc19j7-gate.4zcJEG` worktree and generated assets. The authoritative merge-base-scoped rerun used a fresh cache and reported zero issues.
