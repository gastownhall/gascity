# Release gate: oversized startup-prompt argv fallback

- Deploy bead: `ga-slqjmk`
- Implementation bead: `ga-q8wgom.1.1`
- Review bead: `ga-6lef30`
- Reviewed source: `a05c219cca80599f806c82068865ac201a7c7bd2`
- Source branch: `builder/ga-q8wgom.1.1` (provenance only)
- Planned deploy branch: `deploy/ga-slqjmk-gate`
- Base checked at gate time: `origin/main@a4361e58228b82b668609c19159031baa0d6928c`
- Gate result: **PASS with attributed pre-existing failures**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This record applies
the active deployer release criteria loaded by `gc prime`, the source bead's six
acceptance criteria, and the repository test policy in `TESTING.md`.

## Gate criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Closed review bead `ga-6lef30` records `verdict: pass` for the exact reviewed source `a05c219cca80599f806c82068865ac201a7c7bd2`. Its metadata and deploy handoff both pin that full SHA. |
| 2 | Acceptance criteria met | **PASS** | The pure routing tests cover below-limit behavior, exact raw/quoted thresholds, quote inflation, UTF-8 byte counting, `arg`/`flag`/`none`, ACP, tmux fallback, and unsupported-runtime failure. Prepared-start tests prove unsupported runtimes fail before provider start and that structured logs omit prompt content. `TestDoStartSession_OversizedPromptNudgeFallbackSendsFullContentAfterReady` proves a realistic oversized tmux launch removes the prompt from argv, passes readiness, and submits the byte-exact payload post-start. All named tests passed. |
| 3 | Tests pass | **PASS with attributed failures** | The documented full-scope command, `make test-local-full-parallel`, completed 36/40 jobs PASS and 4/40 jobs raw FAIL, with no test SKIP event observed. Four unique failures reduce to the pre-existing tracked conditions documented below. All 14 diff-owned top-level tests passed; none skipped or failed. `waiver_ref: none`. |
| 3b | Policy, build, and static checks | **PASS** | `make test-ci-policy`, `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed lint-affected`, `go build ./...`, and `go vet ./...` all exited 0. The affected static closure was `./cmd/gc ./internal/runtime/tmux ./scripts` and reported `0 issues`. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, action, matrix, timeout, required-check list, `Makefile`, or `scripts/cipolicy` path changed. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No unresolved HIGH review findings | **PASS** | Review bead `ga-6lef30` records no blocking style, security, specification, or coverage finding; the only style note is explicitly minor/non-blocking. |
| 5 | Final branch clean | **PASS** | `git status --porcelain=v1` produced no output before this checklist was added, and `git diff --check origin/main...a05c219cca80599f806c82068865ac201a7c7bd2` was clean. Cleanliness is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after the already-merged pre-flight found no PR for the reviewed SHA. Current `origin/main` is the merge base; `git rev-list --left-right --count origin/main...a05c219cca` returned `0 4`. `git merge-tree --write-tree --messages origin/main a05c219cca` exited 0 with tree `7ecf25a8cf776a14203056f1a6aa0186c773d9ab` and no conflict messages. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The four reviewed commits and eight changed paths implement one startup-prompt delivery contract: argv-size classification, fallback/error propagation, structured evidence, realistic tmux delivery proof, and its hand-maintained test manifest. No independent feature is bundled. |

## Source and ancestry evidence

The recorded source resolves to the exact full commit used by review and this
gate:

```text
git rev-parse --verify --quiet 'a05c219cca80599f806c82068865ac201a7c7bd2^{commit}'
a05c219cca80599f806c82068865ac201a7c7bd2
```

The reviewed range is four commits:

```text
ccf4fd76ca feat: green — Guard oversized argv prompts and fall back to post-start delivery (refs ga-q8wgom.1.1)
1a6d200f3a test(feat): red — Review: Guard oversized argv prompts and fall back to post-start delivery (refs ga-usohi8)
38b508f308 test(tmux): prove oversized-nudge fallback reaches runtime after readiness
a05c219cca chore(scripts): register oversized-nudge fallback test in tmux manifest
```

Net diff: 8 files, 924 insertions, 28 deletions. Paths are confined to
`cmd/gc`, `internal/runtime/tmux`, and the tmux test manifest under `scripts`.
There are no `.claude/**`, API-schema, dashboard, workflow, or unrelated package
paths.

The mandatory scope guard passed with the deploy bead and the three reviewed
related ancestry IDs:

```text
assert_deploy_ancestry_scope origin/main a05c219cca80599f806c82068865ac201a7c7bd2 \
  ga-slqjmk ga-q8wgom.1.1 ga-usohi8 ga-a1bdd4
```

## Test evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: 36 PASS jobs, 4 raw FAIL jobs, 0 SKIP jobs; 4 unique top-level failing tests/conditions; no `--- SKIP:` event in the 40 job logs
- `full_log`: `/var/tmp/ga-slqjmk-gate.lcXdrf/full-suite.out`
- `shard_logs`: `/var/tmp/gc-local-tests.JmQvPk`
- `skip_justification`: none needed; no test skip was observed
- `waiver_ref`: none
- `ci_lane_run`: n/a — no CI-config change

The rootless Podman 5.8.4 socket was live before the run. The local cache
contained the repository-pinned Dolt `2.1.7` images and
`testcontainers/ryuk:0.14.0`, so container-aware tests did not silently green on
a missing runtime.

### Diff-owned tests

The full runner's green `cmd/gc` and `internal/runtime/tmux` shards enumerated
the candidate's tests by name. The non-verbose `scripts` package ran in the
full suite; a supplemental exact-head named run recorded its five manifest
tests explicitly in `/var/tmp/ga-slqjmk-gate.lcXdrf/diff-owned-scripts.log`.

`diff_tests_executed`:

- `TestPreparedStartOversizedPromptHardFailsBeforeLaunch`: PASS
- `TestPreparedStartOversizedPromptFallsBackOnNudgeCapableRuntime`: PASS
- `TestPreparedStartOversizedPromptByteExactWithEmbeddedQuotesAndNewlines`: PASS
- `TestPreparedStartOversizedPromptHardFailLogsStructuredRecordWithoutPromptContent`: PASS
- `TestPreparedStartOversizedPromptNudgeFallbackLogsStructuredRecordWithoutPromptContent`: PASS
- `TestPromptDelivery`: PASS
- `TestPromptDeliveryOversized`: PASS
- `TestPromptDeliverySupportFor`: PASS
- `TestDoStartSession_OversizedPromptNudgeFallbackSendsFullContentAfterReady`: PASS
- `TestRuntimeTmuxManifestMatchesCanonicalLinuxIntegrationInventory`: PASS
- `TestRuntimeTmuxManifestSixShardsPartitionInventoryExactlyOnce`: PASS
- `TestRuntimeTmuxManifestDiscoveryUsesCanonicalLinuxPlatform`: PASS
- `TestRuntimeTmuxManifestDiscoveryDistinguishesTestMainHarness`: PASS
- `TestRuntimeTmuxManifestDriftDiagnostics`: PASS

### Full-suite failure attribution

| Failure | Tracker | Four-clause attribution |
|---|---|---|
| `TestRepositoryLedgerMatchesCensusAndDocumentation` in `unit-core` and `integration-packages-core-4-of-4` | `ga-cp3hwi.1` | Not diff-owned; tracker predates this run and the sighting was appended. Mechanism proof: the exact failure compares checked-in resource-ledger baselines with bootstrap-policy values, while the candidate changes neither resource-census package nor either baseline. No failing-path overlap. |
| `TestDockerSessionProtocol/context_cancellation_rolls_back_created_container` in `unit-core` | `ga-d5l7kb` | Not diff-owned; tracker predates this run and names the exact empty immutable-ID-cleanup signature. Mechanism proof: the test executes unchanged `scripts/gc-session-docker`, its unchanged fake Docker fixture, and unchanged `internal/runtime/exec`; the candidate's separate tmux manifest tests cannot alter that protocol. No failing-file overlap. |
| `TestBdFlagManifestCurrent` in `integration-packages-core-1-of-4` | `ga-f0uceo` | Not diff-owned; tracker predates this run and records the same installed-`bd` flag surface. Mechanism proof: the result is a comparison between the host `bd --help` output and unchanged `internal/bdflags`; the candidate can alter neither input. No path overlap. |
| `TestE2E_SuspendResume_City` in `integration-rest-full-2-of-8` | `ga-vkhfnj` (consolidated sighting `ga-yc0e3a`) | Not diff-owned; canonical tracker predates this run and the sighting was appended. Proof (d)/cross-run: `ga-yc0e3a` records the identical missing-`citysus.report` timeout on both a candidate and exact `origin/main`, with further unrelated-candidate/base recurrences. The candidate does not touch the test/report/resume paths; this fixture has no prompt template, so its empty prompt cannot enter the new oversized-prompt branches. |

`failure_attribution`: `TestRepositoryLedgerMatchesCensusAndDocumentation -> ga-cp3hwi.1 | clause 3(a) mechanism`; `TestDockerSessionProtocol/context_cancellation_rolls_back_created_container -> ga-d5l7kb | clause 3(a) mechanism`; `TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism`; `TestE2E_SuspendResume_City -> ga-vkhfnj (sighting ga-yc0e3a) | clause 3(d) exact-base reproduction/cross-run`.

## Policy and static evidence

```text
make test-ci-policy                                                         PASS
LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=origin/main make fmt-check-changed lint-affected
                                                                            PASS (0 issues)
go build ./...                                                              PASS
go vet ./...                                                                PASS
```

Logs are under `/var/tmp/ga-slqjmk-gate.lcXdrf/`.

## Release disposition

**Gate PASS.** Create `deploy/ga-slqjmk-gate` from the exact reviewed source,
commit this record, push only the isolated branch, open a PR against `main`,
publish `release-gate/deploy-clearance=success` on the exact PR head, and route
the merge request to mayor/mpr. The deployer does not merge.
