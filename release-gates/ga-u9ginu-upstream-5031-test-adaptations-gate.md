# Release gate: upstream #5031 ambiguous-CWD test adaptations

- Deploy bead: `ga-u9ginu`
- Review bead: `ga-vywcg6` — PASS
- Build bead: `ga-dbjavc`
- Source bead: `ga-3vjwoc`
- Reviewed source: `25f6800bbd33247b8eaf6bf0f67091d66cfc4ac9`
- Base: `origin/main@909879c882ececb519b380f050f1d467a8bb1171`
- Deploy branch: `deploy/ga-u9ginu-gate`
- Deploy mode: `remote`; push remote: `origin`
- Pre-flight: the reviewed source has no associated pull request
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-vywcg6` is closed with verdict `pass` and pins exact source `25f6800bbd33247b8eaf6bf0f67091d66cfc4ac9`. It reports no style, security, specification, or affected-package finding. |
| 2 | Acceptance criteria met | **PASS** | Each ambiguous-CWD fixture now starts one session, clears its transcript key where applicable, kills it without closing its bead, and only then starts the second same-CWD session. This satisfies the live-runtime collision guard while retaining two open keyless beads for the ambiguity assertion. The `absent` subtest remains unchanged. |
| 3 | Tests pass | **PASS with four attributed raw failures** | The isolated 40-job full local CI union completed **36 PASS / 4 raw FAIL / 0 omitted jobs**, comprising **47,183 PASS / 4 FAIL / 227 SKIP** top-level executions. Every diff-owned test passed in both selecting tiers: **6 PASS / 0 FAIL / 0 SKIP**. All four raw failures stopped during unrelated external beads database bootstrap, before their scenario assertions or candidate behavior, and are attributed below to a tracker that predates the run. Build, vet, policy, affected-package lint, formatting, documentation, native dependency, core-boundary, event-isolation, and DoltLite-beads gates pass. `waiver_ref: none`; `ci_lane_run: n/a` because no CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-vywcg6` records no blocker, major, unresolved HIGH, or security finding. The test-only diff adds no dependency, production behavior, auth, network, wire, persistence, or sensitive-logging surface. |
| 5 | Final branch is clean | **PASS** | Before this gate record was written, `git status --porcelain=v1`, `git diff --check origin/main...HEAD`, and the tracked-file formatting check were empty. All generated/policy checks left the tree clean. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 25f6800bbd33247b8eaf6bf0f67091d66cfc4ac9` exited 0 and produced tree `876035540fe1a22991ff4c4d1e06f59d939d9db3`. The candidate is one commit ahead of reviewed base `550240d32d16688b25ca8ecfe203773826a3dedd`; no deployer self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One reviewed test-only commit changes two files for one theme: preserve keyless same-CWD ambiguity coverage while complying with the live-CWD collision guard. The ancestry guard accepts deploy `ga-u9ginu`, review `ga-vywcg6`, build `ga-dbjavc`, and source `ga-3vjwoc`; no unrelated or `.claude/**` change is present. |

## Acceptance evidence

- `TestTranscriptPathClassifiedDistinguishesAbsentFromAmbiguous/ambiguous` kills session `one`, then creates session `two`, and still expects `TranscriptAmbiguous`; `/absent` is unchanged.
- `TestFactorySweepSessionModelUsageKeylessClaudeAmbiguousSettles` clears session `one`'s key, calls `Kill`, starts keyless session `two`, and retains the no-emission/settled assertions.
- `TestDiscoverSweepTranscriptKeylessClaudeAmbiguousSettles` uses the same kill-before-second-start sequence and retains the discovery settlement assertions.
- The fixtures deliberately use `Kill`, not `Close`: the first runtime is released while its bead remains open and participates in ambiguous workdir fallback.
- Candidate scope is two test files, 96 insertions, and 43 deletions. Production files, `go.mod`, and `go.sum` are untouched.

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-u9ginu-gate-20260915T1914/jobs /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope: full-suite`
- `test_counts: 47,183 PASS / 4 raw FAIL / 227 SKIP`; job result `36 PASS / 4 raw FAIL / 0 omitted` out of 40.
- `all_execution_events: 79,797 PASS / 4 FAIL / 313 SKIP`, including subtests.
- `required_job_coverage`: the local union ran `unit-core`, all six local `cmd-gc-process` shards, `productmetrics-testhook`, all package-integration core/cmd-gc/tmux shards, review-formula basic/retry/recovery shards, bdstore, REST smoke, and all eight REST-full shards.
- `diff_tests_executed: 6 PASS / 0 FAIL / 0 SKIP` — each changed top-level test passed once in `unit-core` and once in its selecting integration-package shard:
  - `TestTranscriptPathClassifiedDistinguishesAbsentFromAmbiguous` (including both `absent` and `ambiguous` subtests)
  - `TestFactorySweepSessionModelUsageKeylessClaudeAmbiguousSettles`
  - `TestDiscoverSweepTranscriptKeylessClaudeAmbiguousSettles`
- `skip_justification`: the 227 top-level skips are existing suite-controlled platform, capability, provider-matrix, root-only, helper-process, or opt-in exclusions. None is diff-owned, and every diff-owned test executed in both selecting tiers.
- `waiver_ref: none`
- `ci_lane_run: n/a (no workflow, job, matrix, timeout, or required-check configuration changed)`
- Per-job logs: `/var/tmp/ga-u9ginu-gate-20260915T1914/jobs`
- Per-job log manifest digest: `232fd6b6f057026caf77d1e2dbab8580bd789598d29217ff21acc45cd903dd50`

### Criterion 3a failure attribution

The raw failures remain recorded as failures. Root-cause bead `ga-e2z1zb` existed before this run and tracks the exact random-cursor schema-bootstrap refusal. Each failure occurs in `test/integration`, while this candidate modifies only package-local tests in `internal/session` and `internal/worker`; there is no path overlap. The candidate changes neither beads initialization nor Dolt schema handling, and each fixture stopped before reaching its scenario assertions. This run's four exact sightings were appended to `ga-e2z1zb` and read back successfully.

| Raw failing test | Attribution |
|---|---|
| `TestAdoptPRFormulaCompileAndRun` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped when external bd treated a partially migrated database as empty and refused the resulting v46-to-v66 shared-server migration, before formula compilation or candidate behavior. |
| `TestCleanInstallTutorialPath` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc rig add` stopped on the tracked v54-to-v66 schema-bootstrap refusal before the tutorial assertions. |
| `TestGCLiveContract_BeadsAndEvents` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Rig creation returned 500 when external bd stopped on the tracked v55-to-v66 refusal, before the live contract reached candidate behavior. |
| `TestGraphWorkflowFailureRunsCleanup` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped on the tracked v54-to-v66 refusal before graph execution or cleanup assertions. |

Condition tracker `ga-lejnse` identifies `ga-e2z1zb` as the reproduced root-cause/fix bead: concurrent initialization can expose a partially migrated database, which `op_init` misclassifies as empty before the shared-server migration safety check refuses the random intermediate cursor.

## Static and policy evidence

- `make test-ci-policy` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS (`737` modules; `25` AWS, `9` Azure, `15` DoltHub, `1` Google API; `175,964,063` binary bytes).
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=550240d32d16688b25ca8ecfe203773826a3dedd make lint-affected` with a fresh per-run golangci cache — PASS, `0 issues`. The reviewed merge base is used because comparing the old source directly to current main makes the selector conservatively treat a newer mainline-only release-gate document as deleted and widen to ignored dashboard `node_modules`; those three third-party diagnostics are not tracked source or candidate findings.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed` — PASS.
- `make check-docs` — PASS.
- `go vet ./...` — PASS.
- `go build ./...` — PASS.
- `make check-hooks` — PASS; `.githooks` owns `core.hooksPath`.
- `git diff --check origin/main...HEAD` — PASS.

## Environment integrity

- Rootless Podman was available through `/run/user/1000/podman/podman.sock`; Ryuk was disabled for the full test union as required on this host.
- The pinned `docker.io/dolthub/dolt-sql-server:2.2.0` image was present before the run.
- The repository's isolated-test wrapper supplied the test environment and topology. The shared Go build cache was neither cleared nor redirected.
- No shared Dolt schema migration was attempted; the safety refusals were observed and attributed without mutating the shared server.
