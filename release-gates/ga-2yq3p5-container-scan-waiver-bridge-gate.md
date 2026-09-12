# Release gate: time-boxed container-scan waiver bridge

- Re-gate bead: `ga-hw5v0q`
- Existing deploy bead: `ga-2yq3p5`
- Round-two review bead: `ga-9r8x58`
- Reviewed source: `0f700a13c02772be58a5c3bb101dc5807edebc36`
- Base evaluated: `origin/main@ac8f6c6cdf875feb72064caba3b1346ae93614d2`
- Existing branch: `deploy/ga-2yq3p5-gate`
- Existing pull request: [#5885](https://github.com/gastownhall/gascity/pull/5885)
- Deploy mode: `remote`; push remote: `origin`
- Overall verdict: **PASS with attributed non-diff-owned failures**

This is the round-two re-gate of the already-open pull request. The pull
request head matched the reviewed source before evaluation. Per the bead's
specific instruction, this gate updates the existing isolated branch and pull
request; it does not cut another branch or open another pull request.

`docs/PROJECT_MANIFEST.md` and `work-packages/` are absent at the reviewed
source, so the seven deploy criteria and the reviewed bead's acceptance
contract are authoritative.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | `ga-9r8x58` is closed with `verdict: pass` on the exact reviewed source. The reviewer independently ran the affected scripts package and found no style, security, or specification gap. |
| 2 | Acceptance criteria met | **PASS** | The waiver file contains the complete reviewed E9 set, every entry remains time-boxed to `2026-09-21`, and the owning tests validate the exact CVEs, paths/purls, non-removal rules, expiry, durable-fix statements, and the gate document's entry-count arithmetic. The exact-head Container Scan and `Image vulnerabilities` job are green. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full-scope command scheduled all 40 jobs and produced **49,993 PASS / 6 attributed FAIL / 210 SKIP** top-level results. All six diff-owned tests executed and passed twice. The raw failures are covered by the independently evidenced trackers in the attribution section; none is diff-owned or overlaps a changed path. |
| 3a | Non-diff-owned failures attributed | **PASS** | Five fixture-start failures share the tracked beads#5920 shared-Dolt migration-refusal condition `ga-vkhfnj`. The `citysus.report` timeout exactly matches the proven suspend/wake bug tracked by `ga-dc9utn`. Each tracker predates this run; verified sightings were appended. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy`, affected-file lint and formatting, `go vet ./...`, `go build ./...`, docs synchronization, and diff hygiene all pass. The exact-head remote `CI / required` and `CI / integration` fan-ins are also green. |
| 3c | CI-config lane run | **PASS / n/a** | No workflow, job matrix, timeout, runner policy, required-check list, Makefile, or `scripts/cipolicy/**` path changed. `.trivyignore.yaml` is scanner policy input, and its own exact-head `Image vulnerabilities` lane completed successfully. |
| 4 | No high-severity review findings open | **PASS** | The round-two reviewer recorded `style_findings: none`, `security_findings: none`, `uncovered_criteria: none`, and `verdict: pass`. |
| 5 | Final branch is clean | **PASS** | The exact reviewed source was clean before and after the full suite and static lanes. This checklist update is the deployer's only working-tree change. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree ac8f6c6cdf875feb72064caba3b1346ae93614d2 0f700a13c02772be58a5c3bb101dc5807edebc36` exited 0 and produced tree `819768538e3a5e02c5db9f3deb8d424d5cf27ad3`. Divergence was 5 base-only / 11 candidate-only commits. No self-rebase was needed. |
| 7 | Single feature theme | **PASS** | The cumulative branch changes one security-policy surface: temporary, narrowly scoped container-scan waivers plus tests and this gate record. No production implementation, dependency pin, image recipe, or independent behavior rides along. |

## Acceptance evidence

- `.trivyignore.yaml` contains 64 entries: 47 pre-existing entries (45
  original, plus the `CVE-2026-46600` entry from the first widening, plus the
  `CVE-2026-56854` entry from docket D6) and 17 further new entries from the
  docket E9 widening.
- `TestReleaseGateWaiverBridgeDocEntryCountMatchesTrivyIgnore` parses the
  preceding sentence, verifies `47 + 17 = 64`, parses the YAML, and verifies
  the actual entry count is 64. This closes the sole round-one review gap.
- Every waiver expires on `2026-09-21` and carries a non-empty durable-fix
  statement.
- The E9 entries cover the remaining HIGH/CRITICAL findings from the prior
  failing scan: x/mod for `gh`; gRPC for `gh`, `dolt`, `bd`, and `gc`; thrift
  for `dolt` and `bd`; Go standard-library findings for `kubectl`; and the
  purl-scoped util-linux and GitPython findings in the MCP mail image.
- No Go standard-library waiver was added for the rebuilt `gh`, `dolt`, or
  `bd` binaries. No prior waiver was removed.
- The cumulative diff against the evaluated base contains only:

  ```text
  .trivyignore.yaml
  release-gates/ga-2yq3p5-container-scan-waiver-bridge-gate.md
  scripts/container_tool_security_test.go
  ```

- The exact reviewed head passed [Container Scan run 34667459615](https://github.com/gastownhall/gascity/actions/runs/34667459615), including [Image vulnerabilities job 103482216324](https://github.com/gastownhall/gascity/actions/runs/34667459615/job/103482216324).

## Full-suite evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel`
- `test_cmd_scope`: `full-suite`
- `test_counts`: **49,993 PASS / 6 attributed FAIL / 210 SKIP** top-level results
- `shard_result`: **35/40 jobs PASS; 5/40 jobs contain the six attributed failures**
- `shard_logs`: `/var/tmp/gc-local-tests.OC4cNi`
- `diff_tests_executed`: all six listed below passed in both full-suite jobs that execute the scripts package
- `waiver_ref`: none
- `ci_lane_run`: n/a for CI configuration; the scanner-policy lane itself passed on the exact reviewed head
- `skip_justification`: the 210 skips are existing helper-process sentinels,
  platform guards, duplicate-lane guards, and opt-in provider/infrastructure
  coverage (including K8s, PostgreSQL, persistence, and live herdr cases).
  None is diff-owned. The container environment was configured before the
  suite, so no container-backed diff-owned test was silently skipped.

Diff-owned tests:

- `TestTrivyIgnoreRefreshesBridgeHorizonAndWaivesXNetDNSMessageCVE`: **PASS**
- `TestTrivyIgnoreWaivesXCryptoSSHCVEForGHDoltBD`: **PASS**
- `TestTrivyIgnoreWidensDocketE9WaiverForRemainingHighCriticalFindings`: **PASS**
- `TestReleaseGateWaiverBridgeDocEntryCountMatchesTrivyIgnore`: **PASS**
- `TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools`: **PASS**
- `TestRebuiltToolsAssertPatchedGRPCArtifact`: **PASS**

The exact reviewed head also passed [CI run 34667459547](https://github.com/gastownhall/gascity/actions/runs/34667459547), including [CI / integration](https://github.com/gastownhall/gascity/actions/runs/34667459547/job/103482754935) and [CI / required](https://github.com/gastownhall/gascity/actions/runs/34667459547/job/103482792108).

### Raw failure attribution

The mandatory non-diff-owned-failure protocol was read before attribution.
All six tests are outside the diff, have no changed-path overlap, and cannot
reach candidate production code because this candidate changes no production
code.

- `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` → `ga-vkhfnj`: bd init refused 18 pending shared-server migrations (`v48` → `v66`).
- `TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries` → `ga-vkhfnj`: fixture gc init refused 11 pending shared-server migrations (`v55` → `v66`).
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` → `ga-vkhfnj`: fixture gc init refused 2 pending shared-server migrations (`v64` → `v66`).
- `TestCleanInstallTutorialPath` → `ga-vkhfnj`: fixture gc init refused 10 pending shared-server migrations (`v56` → `v66`).
- `TestGCLiveContract_BeadsAndEvents` → `ga-vkhfnj`: rig creation returned HTTP 500 because bd init refused 35 pending shared-server migrations (`v31` → `v66`).
- `TestE2E_SuspendResume_City` → `ga-dc9utn`: exact tracked 95-second missing-`citysus.report` signature. The tracker contains a standalone reproduction and the proven reconciler suspend/wake mechanism; this candidate does not touch that path.

`ga-vkhfnj` contains the clean-`origin/main` reproduction for the shared-Dolt
migration-refusal condition. `ga-dc9utn` records that the suspend/resume failure
reproduces alone and is not load-sensitive. Both trackers predate this run, and
this run's sightings were appended and read back before attribution.

## Policy and static evidence

```text
make test-ci-policy                                                   PASS
go vet ./...                                                          PASS
go build ./...                                                        PASS
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make lint-affected
                                                                      PASS (0 issues)
LINT_CHANGED_REF=origin/main LINT_CHANGED_SCOPE=tracked make fmt-check-changed
                                                                      PASS
make check-docs                                                       PASS
git diff --check origin/main...HEAD                                   PASS
git config core.hooksPath                                             .githooks
```

The test environment was prepared before criterion 3: rootless Podman 5.8.4,
the user socket at `/run/user/1000/podman/podman.sock`, Ryuk disabled, and the
cached test images present at the tags pinned by the test code.

## Scope and ancestry audit

`assert_deploy_ancestry_scope` found no denylisted `.claude/**` path. Its
commit-message check returned 21 for two commits that do not cite a bead ID:

- `62ee848027`: the prior deployer's release-gate checklist, touching only this
  file.
- `74575bed56`: a test-only extension of the E9 waiver drop-guards, touching
  only `scripts/container_tool_security_test.go`.

This is an existing, already-published isolated PR branch, so rewriting those
commit messages would require a forbidden force-push. Full diff inspection
proves both commits belong to the same waiver-policy theme; neither introduces
an unrelated subsystem or user-facing behavior. The specific re-gate bead
therefore directs updating this branch in place.

## Release disposition

**Gate PASS.** Commit this updated checklist on the existing
`deploy/ga-2yq3p5-gate` branch, update pull request #5885, publish deploy
clearance on the resulting exact head, and route the merge request to mayor.
Merge authority remains with mayor/mpr; the deployer does not merge.
