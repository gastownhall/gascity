# Release gate: execgrace cancellation outcome

- Deploy bead: `ga-bz8hvm`
- Review bead: `ga-7wzlna` — PASS
- Build bead: `ga-kcdabs.1`
- Reviewed source: `c03059a3df88f0af0dfad431482caadfda7dbd8b`
- Base: `origin/main@9700d9a48fb35a2063e1fcb9ee49ab664df26de0`
- Deploy mode: `remote`; push remote: `origin`
- Pre-flight: the reviewed source has no associated pull request
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-7wzlna` is closed with verdict `pass` and pins exact source `c03059a3df88f0af0dfad431482caadfda7dbd8b`. Its style, security, specification, and affected-package checks report no blocking finding. |
| 2 | Acceptance criteria met | **PASS** | `Apply` now returns a zero-value-safe `CancelResult`; `Delivered` preserves the existing caller contract and `Outcome` distinguishes no delivery, process-group interrupt, leader-only fallback, and direct force-kill fallback. Unix and Windows signal helpers return the classification without changing their signaling order. The runtime exec provider makes the single mechanical `Load` to `Delivered` migration. Deterministic Unix seams cover both formerly unobservable fallbacks. |
| 3 | Tests pass | **PASS with five attributed raw failures** | The isolated 40-job full local CI union completed **35 PASS / 5 raw FAIL / 0 omitted jobs**, comprising **47,186 PASS / 5 FAIL / 227 SKIP** top-level executions. Every diff-owned test passed in both selecting tiers: **10 PASS / 0 FAIL / 0 SKIP**. Four raw failures stopped in unrelated beads initialization before candidate behavior; one stopped in an external herdr pane-availability check. All are attributed below to trackers that predate the run. Path-selected worker phase 1 and phase 2 matrices, acceptance A, the minimum-supported bd contract, generated-artifact checks, build, vet, and policy/static gates pass. `waiver_ref: none`; `ci_lane_run: n/a` because no CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-7wzlna` records no blocker, major, unresolved HIGH, or security finding. The new syscall seams are unexported and the diff adds no dependency, network, auth, wire, or sensitive logging surface. |
| 5 | Final branch is clean | **PASS** | Before this gate record was written, `git status --porcelain=v1`, `git diff --check origin/main...HEAD`, and changed-file formatting checks were empty. The generated-artifact checks also left the tree clean. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main c03059a3df88f0af0dfad431482caadfda7dbd8b` exited 0 and produced tree `3fb9608d4925518093ef39a0c707fa70d51b899a`. The candidate is one commit ahead of the recorded base, so no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | One reviewed commit changes eight files for one theme: expose the cancellation-delivery branch, adapt the sole bool consumer, test the two fallback branches, and raise the checked resource census by the two added subprocess tests. The ancestry guard accepts only deploy `ga-bz8hvm`, review `ga-7wzlna`, and build `ga-kcdabs.1`; no unrelated or `.claude/**` change is present. |

## Acceptance evidence

- `CancelOutcome` has four documented values: `CancelNotDelivered`, `CancelGroupSignaled`, `CancelLeaderSignaledOnly`, and `CancelForceKilled`.
- `CancelResult.record` stores the outcome before publishing delivery, so a caller that observes `Delivered() == true` can read the recorded outcome.
- Successful group interruption records `CancelGroupSignaled`; os/exec's later `WaitDelay` escalation does not rewrite that delivery classification.
- A failed `Getpgid` followed by a successful leader interrupt records `CancelLeaderSignaledOnly`.
- A failed group signal followed by a successful direct kill records `CancelForceKilled`.
- An already-finished or never-started process retains the zero-value `CancelNotDelivered` result.
- Windows retains its existing direct-signal-then-kill behavior while conforming to the typed result signature.
- `internal/runtime/exec` continues to let accepted cancellation win over the command's own exit status by checking `Delivered()`.
- The explicitly out-of-scope tmux and herdr production paths are unchanged.
- Candidate scope is eight files, 214 insertions, and 43 deletions; `go.mod` and `go.sum` are untouched.

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-bz8hvm-test.iq2tR4/jobs /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope: full-suite`
- `test_counts: 47,186 PASS / 5 raw FAIL / 227 SKIP`; job result `35 PASS / 5 raw FAIL / 0 omitted` out of 40.
- `required_job_coverage`: the local union ran `unit-core`, all six local `cmd-gc-process` shards, `productmetrics-testhook`, all package-integration core/cmd-gc/tmux shards, review-formula basic/retry/recovery shards, bdstore, REST smoke, and all eight REST-full shards. This is the local sharding equivalent of the path-selected `cmd/gc process` and `Integration` CI jobs.
- `worker_required_job_coverage`: because `internal/runtime/**` changed, all four exact CI profiles (`claude/tmux-cli`, `codex/tmux-cli`, `cursor/tmux-cli`, and `gemini/tmux-cli`) passed both `make test-worker-core` and `make test-worker-core-phase2-all`, including phase-2 real-transport coverage in `cmd/gc`.
- `preflight_required_job_coverage`: `make test-acceptance`, generated dashboard/OpenAPI/reference-doc freshness, and `make test-bd-cli-contract` against the checksum-pinned minimum-supported bd `v1.0.4` all passed.
- `diff_tests_executed: 10 PASS / 0 FAIL / 0 SKIP` — each of the five changed top-level tests passed once in `unit-core` and once in `integration-packages-core-3-of-4`:
  - `TestApplyLeaderSignaledOnlyWhenGetpgidFails`
  - `TestApplyForceKilledWhenGroupSignalFails`
  - `TestApplyAcceptedFlag`
  - `TestApplyTrapRunsBeforeKill`
  - `TestApplyForceKillsUncooperative`
- `skip_justification`: the 227 skips are existing suite-controlled platform, capability, helper-process, live-provider, or opt-in exclusions. The full union also ran the process and integration tiers that own those boundaries. None is diff-owned.
- `waiver_ref: none`
- `ci_lane_run: n/a (no workflow, job, matrix, timeout, or required-check configuration changed)`
- Per-job logs: `/var/tmp/ga-bz8hvm-test.iq2tR4/jobs`
- Per-job log manifest digest: `0f3e3edb571af5687aa4a57a45ef8d0d83f98596c4ed19b3b4baa8a3cda56a84`
- Worker logs: `/var/tmp/ga-bz8hvm-test.iq2tR4/worker-core-exact` and `/var/tmp/ga-bz8hvm-test.iq2tR4/worker-phase2`
- Acceptance and policy logs: `/var/tmp/ga-bz8hvm-test.iq2tR4/{acceptance-a,bd-v1.0.4-contract,static-gates,static-remaining,generated}.log`

### Criterion 3a failure attribution

The raw failures remain recorded as failures. Each tracker existed before this run, the failed path is causally disjoint from the candidate behavior, no failing test is diff-owned, and the candidate's two added subprocess tests cannot supply either prerequisite for the observed external conditions.

| Raw failing test | Attribution |
|---|---|
| `TestProviderLiveClaudeKindPath` | `failure_attribution: ... -> ga-iepsvr | clause 3(a): MECHANISM — PASS`. External `herdr agent start` refused target pane `w1:p1` as `agent_pane_busy` before starting the provider. The concrete test configuration has no `PreStart`; the only herdr use of `execgrace.Apply` is in the unentered setup-command path. The candidate's new tests spawn only local `sleep` processes and cannot own a herdr/tmux pane. `clause-4-guard: same_package=no proof=a added_test_load=yes`. |
| `TestAdoptPRFormulaCompileAndRun` | `failure_attribution: ... -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped when bd refused 37 migrations from v29 to v66, before formula compilation or candidate cancellation code. The candidate creates no city, bd process, Dolt connection, or competing initializer. |
| `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` | `failure_attribution: ... -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped on the tracked concurrent-initializer signature, v45 to v66, before retry behavior or candidate cancellation code. The candidate cannot be either initializer. |
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `failure_attribution: ... -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped on the same tracked condition, v65 to v66, before the recovery formula or runtime exec path ran. The candidate cannot be either initializer. |
| `TestGCLiveContract_BeadsAndEvents` | `failure_attribution: ... -> ga-lejnse / ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Rig creation returned 500 because external `bd init` refused v65 to v66 before the live contract reached candidate behavior. The candidate cannot create or migrate that database. |

The four schema refusals match condition tracker `ga-lejnse`, whose notes identify reproduced root-cause/fix bead `ga-e2z1zb`: two concurrent initializers can expose a partially migrated database and trigger the shared-server safety refusal. The exact sightings above were appended to `ga-lejnse` and read back successfully. The herdr sighting was likewise appended to `ga-iepsvr` and verified.

## Static and policy evidence

- `make test-ci-policy` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS.
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed` — PASS.
- `make check-docs` — PASS.
- `go vet ./...` — PASS.
- `make check-hooks` — PASS; `.githooks` owns `core.hooksPath`.
- `git diff --check origin/main...HEAD` — PASS.
- `make build`, `./bin/gc version`, and `./bin/gc --help` — PASS; version reported `dev`.
- `make dashboard-ci`, `make spec-ci`, and `./scripts/check-generated-docs-drift.sh` — PASS and left no drift.
- The first `lint-affected` run replayed 72 diagnostics from the ambient golangci-lint cache, including 62 paths under nonexistent sibling worktree `/var/tmp/ga-82s3eo-eval`. The identical full affected-package selection with a fresh `GOLANGCI_LINT_CACHE` passed with `0 issues`. This is the pre-existing cross-worktree cache condition tracked by `ga-039od0`; this run's occurrence was appended and verified. Logs: `/var/tmp/ga-bz8hvm-test.iq2tR4/static-gates.log` and `/var/tmp/ga-bz8hvm-test.iq2tR4/lint-affected-clean-cache.log`.

## Environment integrity

- Rootless Podman 5.8.4 was available through `/run/user/1000/podman/podman.sock`; Ryuk was disabled for the full test union as required on this host.
- The required Dolt 2.1.7 images were present before the run.
- The repository's isolated-test wrapper supplied the test environment and topology. The shared Go build cache was neither cleared nor redirected.
- No shared Dolt schema migration was attempted; the safety refusals were observed and attributed without mutating the shared server.
