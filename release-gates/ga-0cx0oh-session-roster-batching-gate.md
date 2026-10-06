# Release gate: batched agent-list runtime reads

- Deploy bead: `ga-0cx0oh`
- Build bead: `ga-n3gt3w`
- Reviewed source: `2dee413d26bea34915dd7c552e53eb283d6c50c5`
- Base: `origin/main@3e686dace5151e510104087f7e867d21f421fa6c`
- Deploy mode: `remote`
- Evaluated: 2026-08-25

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-sd37o0` is closed with an explicit first-pass `Review verdict: PASS` for the resolved source commit. |
| 2 | Acceptance criteria met | **PASS** | `runtime.SessionRosterProvider` and `runtime.EnvironmentBatchProvider` are optional interfaces, so the base provider contract is unchanged. The tmux provider implements both; a missing tmux server returns an empty roster. `/v0/agents` resolves each optional batch provider once, preserves per-session fallbacks for other providers, and gates suspended metadata lookup on a running session. The tmux shard manifest includes the new roster tests and its partition invariant remains covered. |
| 3 | Tests pass | **PASS** | The documented full local CI union ran at the reviewed source with the rootless Podman test environment enabled. It completed **34 PASS / 6 FAIL / 0 SKIP jobs**. All six red jobs are pre-existing, tracked failures covered by prior merge-authority adjudication or standing authorization; the raw failures and attribution are preserved below. All seven diff-owned tests independently report PASS. |
| 4 | No high-severity review findings open | **PASS** | Reviewer recorded no security, correctness, or specification finding and no unresolved HIGH finding. Independent gate inspection found none. |
| 5 | Final branch is clean | **PASS** | `git status --short --branch` was clean at the reviewed source before this gate record was created; `git diff --check origin/main...HEAD` passed. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 2dee413d26bea34915dd7c552e53eb283d6c50c5` exited 0 and produced tree `57fa64b7736565daa6bc38fba1c61fd9679356ae`; no content conflict exists against the fetched base. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The commit range is one cohesive API/runtime optimization: batch tmux session-roster and environment reads used by agent listing, with its tests and shard inventory. The historical gate record in the range documents the same feature. |

## Criterion 3 evidence

- `test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true LOCAL_TEST_JOBS=4 GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 34 PASS / 6 FAIL / 0 SKIP runner jobs`
- `skip_justification: none (0 skipped jobs and no SKIP result in the union logs)`
- `waiver_ref: ga-n3gt3w mayor adjudication dated 2026-08-21 for the named host tmux and bd-manifest failures; ga-lpfjhc mayor standing authorization dated 2026-08-18 for the exact gastownhall/beads#4566 dirty-schema signature`
- Full-suite summary: `/var/tmp/ga-0cx0oh-test-local-full-parallel.log`
- Per-job logs: `/var/tmp/gc-local-tests.Ewg2qW/`

### Diff-owned tests

Supplementary name-resolved run: `go test -count=1 -v ./internal/api ./internal/runtime/tmux ./scripts -run '^(TestSessionRoster|TestSessionRoster_NoServerReturnsEmpty|TestSessionRoster_AbsentNameIsNotRunning|TestAgentListSuspendedCheckGatedByRunning|TestAgentListPrefersBatchSessionRosterAndEnvironment|TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory|TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce)$'`

`diff_tests_executed: 7 PASS / 0 FAIL / 0 SKIP`

- `TestSessionRoster` — PASS
- `TestSessionRoster_NoServerReturnsEmpty` — PASS
- `TestSessionRoster_AbsentNameIsNotRunning` — PASS
- `TestAgentListSuspendedCheckGatedByRunning` — PASS
- `TestAgentListPrefersBatchSessionRosterAndEnvironment` — PASS
- `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory` — PASS
- `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce` — PASS

### Attributed and authorized full-suite failures

- `TestBdFlagManifestCurrent` — raw FAIL, attributed to pre-existing tracker `ga-gqxh5s`, which predates this run and covers the installed-bd flag-manifest skew. Clause 3 mechanism proof: the candidate cannot alter the external `bd --help` surface or `internal/bdflags`, and there is no path overlap. This exact failure is also cleared by the prior mayor adjudication on `ga-n3gt3w`.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` — raw FAIL under the host's missing default tmux bindings, tracked by pre-existing `ga-sxinl6`. These are the exact named failures cleared by the mayor adjudication on `ga-n3gt3w`; the diff-owned `TestSessionRoster*` tests in the same runtime shards all passed.
- `TestAdoptPRFormulaCompileAndRun`, `TestPersonalWorkFormulaCompileAndRun`, and `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` — raw FAIL during fixture initialization with the exact `gastownhall/beads#4566` pending-schema-migrations dirty-table signature. Pre-existing tracker `ga-lpfjhc` carries the mayor's standing authorization. The occurrence was logged on that tracker before gate disposition. The candidate cannot reach Dolt schema migration or store bootstrap, and each failure occurs before the candidate runtime/API path executes.

No raw failure is rewritten as green; the release criterion passes because each failure is independently tracked and either specifically adjudicated or covered by a prior standing authorization.

## Policy and static lanes

- `policy_lane: make test-ci-policy` — **PASS** on the exact synthetic PR merge tree.
- `policy_lane: LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make lint-affected` — initial shared-cache run surfaced only diagnostics from deleted `/var/tmp` worktrees, the exact defect tracked by pre-existing `ga-u8z8j6`; no candidate path was named. The same required target rerun with an isolated on-disk `GOLANGCI_LINT_CACHE` completed **PASS, 0 issues**.
- `policy_lane: make fmt-check-changed` — **PASS** on the exact synthetic PR merge tree.
- `go vet ./...` — **PASS**.
- `go build ./...` — **PASS**.
- `make dashboard-ci` — **PASS**. The change modifies handler implementation but no API schema, OpenAPI artifact, generated client, or dashboard asset; a preview smoke was therefore not required.
- Git hooks are active through `/home/jaword/projects/gascity/.githooks`.

The synthetic PR merge used base `3e686dace5151e510104087f7e867d21f421fa6c`, reviewed source `2dee413d26bea34915dd7c552e53eb283d6c50c5`, and tree `57fa64b7736565daa6bc38fba1c61fd9679356ae`. It was used only for base-aware static/policy evaluation; the deploy branch is cut from the authoritative reviewed source.
