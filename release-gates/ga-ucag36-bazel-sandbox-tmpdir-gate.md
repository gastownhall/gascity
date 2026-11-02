# Bazel fork-PR sandbox directory release gate — ga-ucag36

Reviewed deploy source: `afc7c89bc1859a385d045f9581ff531acb5017f4`.
Base: `4e9ae8b03c5062884cc66b81166f53d3ba105114`.
Materialized merge: `df3ba661c3af57db24b58dd2654159babc10f3ff`; verified tree `2f2fc4ad7094fa82c98e1ee3f9146cca0eb1dbf9`.

**Verdict:** **PASS**

Date: 2026-09-28. Review: ga-9lyxr1. Build: ga-45dw81. PR: https://github.com/gastownhall/gascity/pull/6801.
DEPLOY_MODE: remote; PUSH_REMOTE: fork; branch: deploy/ga-ucag36-gate. No waiver.

| Criterion | Local evidence |
|---|---|
| 1 | PASS: ga-9lyxr1 reviewer PASS; message-only carryover e15ddc5201c361768ffda0b11d01bf17548a27a9 to recorded source independently recomputed at stable patch-id 1f17932e965f15ec9951f81ef41ce2787f850e39, empty endpoint diff. |
| 2 | PASS: setup creates /tmp/bt before both Bazel paths; sibling workflow search has no other bazel test invocation; .bazelrc unchanged. First real fork/non-RBE run executed all 182 tests, all passed, with zero /tmp/bt I/O exceptions. |
| 3 | PASS with the attributed failures below: documented full-suite command completed all 40 jobs, raw exit 2; 96,737 PASS / 14 FAIL / 330 SKIP completion rows. Diff-owned parent PASS in unit and integration. Four conditions attributed below. CI criterion 3c PASS: real fork lane completed, 182/182 tests PASS, BUILD sync PASS. |
| 4 | PASS: zero open HIGH findings on review ga-9lyxr1. |
| 5 | PASS locally: source and merged worktrees clean; scratch checked for owned running processes and removed after all commands ended. |
| 6 | PASS: original full-suite merge clean; resume re-fetched origin/main at 5f559b68cd202a81697865ace46ef397896ab404, materialized merge 0560034f3a616438bb8e9249e68b3366051945a5, verified exact tree 28463ac6fe5c47d9608c6618965a4ae849d613c9, and independently ran full go build ./... plus go vet ./..., exit 0. No self-rebase needed. |
| 7 | PASS: one theme, creating Bazel sandbox tmpdir plus its regression test and generated BUILD source entry. Ancestry guard accepts only ga-ucag36/ga-45dw81/ga-9lyxr1, no stack. |

`test_cmd_scope: full-suite`; `test_cmd: make test-local-full-parallel LOCAL_TEST_JOBS=4`; both load-gate-run.sh and isolated-test-run.sh used. No TRIPWIRE. `waiver_ref: none` (Gas City has no waiver path). Counts include subtests and repeated profiles, not distinct test parents.

## Load telemetry

| Lane | Threshold | Wait seconds | Timed out | Start | Max | Mean | Samples | Read errors |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| Primary full suite | 15 | 0 | 0 | 14.12 | 50.91 | 30.06 | 96 | 0 |
| Tier A acceptance | 15 | 1801 | 1 | 20.40 | 50.91 | 32.01 | 68 | 0 |
| Complement matrix | 15 | 1801 | 1 | 21.78 | 50.60 | 31.62 | 82 | 0 |
| First minimum-bd contract | 15 | 0 | 0 | 13.86 | 13.86 | 13.86 | 1 | 0 |

## Failure attribution

All four trackers were opened during this run and read back again on resume, predate the test execution, and had new sightings recorded and read back. All satisfy clauses 1/2/4 and mechanism proof 3(a): no changed failing test file/package; actual merged test import closures exclude scripts; workflow/BUILD do not execute in these Go paths. No production Go change, census bump, new test target, subprocess/sleep/server or added parallelism. The sole new test uses strings/testing inside an existing scripts target. Own build ga-45dw81 is stamped gc.fixes_tracker=ga-ymtsrk. Each blocking condition has a separately resolved, stamped, unlanded repair, so repeat attribution qualifies. Raw failures remain FAILs.

| Test failure row | Elapsed | Log | Tracker |
|---|---:|---|---|
| `TestBuildDesiredState_MinZeroDefaultScaleCheckRoutedWorkCreatesPoolSession` | 15.02s | `cmd-gc-process-6-of-6.log` | ga-aik16g |
| `TestBdStoreMailWispInsert` | 16.29s | `integration-bdstore.log` | ga-1037rg |
| `TestBdStoreConditionalWriterConformance` | 10.31s | `integration-packages-core-3-of-4.log` | ga-1037rg |
| `TestBdStoreConditionalWriterConformance/scaffold_roundtrip_any_bd` | 4.83s | `integration-packages-core-3-of-4.log` | ga-1037rg |
| `TestBdStoreReleaseIfCurrentAgainstRealBd` | 9.00s | `integration-packages-core-3-of-4.log` | ga-1037rg |
| `TestBdStoreDeleteBatchOrphansExternalDependents` | 11.36s | `integration-rest-full-1-of-8.log` | ga-1037rg |
| `TestGastown_MailArchive` | 2.77s | `integration-rest-full-1-of-8.log` | ga-4w6d2r |
| `TestGastown_PipelineMailChain` | 17.17s | `integration-rest-full-1-of-8.log` | ga-4w6d2r |
| `TestDoltConfigWiringExternalHost` | 16.22s | `integration-rest-full-2-of-8.log` | ga-1037rg |
| `TestMail_BashAgent` | 18.10s | `integration-rest-full-2-of-8.log` | ga-4w6d2r |
| `TestHumaBinary_CityCreateAsync` | 24.46s | `integration-rest-full-5-of-8.log` | ga-n75ap3 |
| `TestGastown_MailRoundTrip` | 17.92s | `integration-rest-full-6-of-8.log` | ga-4w6d2r |
| `TestGastown_PipelineMailAndWork` | 17.24s | `integration-rest-full-6-of-8.log` | ga-4w6d2r |
| `TestGastown_PipelineConvoyTracking` | 2.86s | `integration-rest-full-8-of-8.log` | ga-4w6d2r |

| Condition | Unlanded designated fix | Resolved commit |
|---|---|---|
| eventkit.lock fixture cleanup | ga-8t1yid / ga-aik16g | b10f83121e03d767ebbb64cf513f71b6ffca73e9 |
| HOME/shared-server leakage | ga-wapfnm / ga-1037rg | 93496f512368bac2077119ecddd3b0bbcf56420b |
| hard-coded bead-prefix fixtures | ga-c2atlu / ga-4w6d2r | 66b9671a38491c723479ee56d13fc3cb3bfb8732 |
| bd writable reinit preflight migration timeout (#5920) | ga-h8haz6 / ga-n75ap3 | c98dbc740ee61b86226bf29a686cfa37bb3f3d62 |

Landing checks at the time of the failed tests returned NOT-LANDED, exit 1, against the captured origin/main base in this run. HOME minimum-bd diagnosis also verified against ga-od3t3e's six-way reproduction; #5920 diagnosis uses ga-k8l7y9/FINDINGS.md, correcting superseded multi-initializer speculation.

## Supplementary lanes and policy

- Minimum-bd v1.0.4 contract: exit 2, 1 PASS / 36 FAIL / 0 SKIP; repeated complement invocation recorded separately with the same counts. HOME/runBD config failure attributed to ga-1037rg.
- Tier A acceptance: exit 2, root TestMain schema probe fails before m.Run (database beads absent on host 3308), ga-1037rg; helper package separately 61 PASS / 0 FAIL / 0 SKIP. Root test bodies unexercised.
- Five required acceptance selections (proxied default, user shared-server isolation, migrations, M1/M5 topology, proxied-native): each exit 1 on the same pre-m.Run HOME/schema probe; zero root completions. No root acceptance PASS claimed.
- Worker phase2 claude, codex, cursor, gemini: each exit 0, 51 PASS / 0 FAIL / 0 SKIP. Release-config goreleaser check exit 0.
- Policy lanes all exit 0: pinned lint-affected (GLT_RECORD pin/resolved 2.12.0, mismatch=false, zero issues), fmt-check-changed, test-ci-policy, check-gomod-replace, check-native-dependency-surface, check-eventexport-isolation, check-core-boundary, test-native-doltlite-beads, check-docs.
- Generated lanes all exit 0: dashboard-ci, spec-ci with required oapi generator, reference-docs drift, dashboard test/e2e types, Vitest (932 PASS, 96 files), browser e2e (19 PASS / 0 FAIL / 0 SKIP). Generation checkout remained clean and was removed.
- Primary bd v1.3.0 and Dolt 2.1.7 pinned before PATH; minimum bd1.0.4 checksum verified. Rootless Podman socket and pinned cached images verified; paired Ryuk opt-out under installed stale-container sweep.

## Skips

330 raw skip rows retained by name and context in results.json. They are outside the changed test file. They cover unit-lane safety exclusions re-exercised in the full process/integration union; platform-only paths; helper-process entry points; optional external PostgreSQL/br/live-registry fixtures; opt-in real-tmux destructive/live reproductions; ambient-CWD safety characterization; unsupported bd conditional revision capability; missing fixture-specific API identities; optional persistence probes. A capability skip is unexercised, not a PASS. No diff-owned skip occurred. Detailed per-name contexts are retained in results.json and skip-justifications.md.

Raw evidence: `/var/tmp/deploy-ga-ucag36.6UJNqe/` (full-suite.log, shards/, attributed-results.json, complement-counts.json, policy/generated logs and return codes, import closures, landing proofs).


## First real changed-workflow run

`ci_lane_run: https://github.com/gastownhall/gascity/actions/runs/36476656141 — PASS — draft-PR-triggered pre-PASS`

The run belongs to PR #6801 by its own Bazel check links. REST `pull_requests=[]` on this fork run was handled by reading the PR-number checks and verifying the workflow path, source head, and quad341/gascity head repository. `.github/workflows/bazel-test.yml` completed successfully. Both `bazel test (side-by-side)` and `BUILD files in sync` jobs passed. The remote execution and pool prewarm steps skipped because this is a fork PR, so this is the required local/non-RBE path.

The raw job log reports `Executed 182 out of 182 tests: 182 tests pass.` It contains zero `/tmp/bt` I/O exceptions. The setup step successfully created the directory. The advisory critical-path report printed `BUDGET: FAIL — 1483.0s > 150s (T2)`; its existing workflow command uses `|| true`, and it is an optimization metric. The test step completed with success; it did not time out or execute partially.

The unrelated `.github/workflows/ci.yml` run 36476656222 initially failed before listing cmd/gc tests in shards 1/4: dependency ZIP downloads from proxy.golang.org returned HTTP/2 INTERNAL_ERROR. Its already-triggered attempt 2 completed successfully on the same PR source. The evidence watchdog also started a fresh evaluation. These retries were already present when this session resumed; the deployer did not trigger them.

## Required coverage mapping

The local full-suite command covers the fast unit baseline, all cmd/gc process shards, integration package shards, bdstore, and full integration-rest shards (all 40 jobs reached terminal state). These correspond to CI's `Preflight / unit cover`, `cmd/gc process`, and `Integration / packages-*`, `Integration / bdstore`, and `Integration / rest-*` jobs. The supplementary commands above cover `Contract / bd CLI (minimum supported)`, `Preflight / acceptance A`, `Contract / acceptance A (bd current)`, `Beads / topology acceptance`, `Beads / proxied-native acceptance`, `Worker core phase 2`, `Release config`, `Preflight / static checks`, `Preflight / generated artifacts`, and `Dashboard SPA`. The ordinary worker-core proofs are also in the full local suite. The full local scope and the limitations of the attributed acceptance failures are recorded separately from CI's success.

The changed workflow's own `bazel test (side-by-side)` and `BUILD files in sync` results are additional real CI evidence, independent of the new YAML regression test. No local focused acceptance selection is offered as the primary full-suite command.

The requested docs/PROJECT_MANIFEST.md is absent from the reviewed tree and current main. Criteria are the supplied release-gate-criteria protocol plus the build bead's recorded exit contract.

The mayor's 19:43Z host cleanup removed test-polluted ~/.beads/config.yaml and stopped the orphan 3308 server. The full-suite failures above were captured before that host intervention. They remain raw historical failures from this gate, with the existing mechanism proofs and fix lineage. No post-cleanup reproduction or green replacement run is claimed.


## Final merge-tree verification and handoff boundary

The source did not move during recovery. Reviewed-original and repaired source were re-resolved; their independently recomputed patch IDs match, with empty endpoint diff. The unchanged ancestry/denylist guard passes with accepted lineage ga-ucag36, ga-45dw81, ga-9lyxr1 and no live stack.

After upstream main advanced by three commits, the merge result at 5f559b68cd202a81697865ace46ef397896ab404 was materialized again as 0560034f3a616438bb8e9249e68b3366051945a5, tree 28463ac6fe5c47d9608c6618965a4ae849d613c9. Full `go build ./...` and `go vet ./...` completed with exit 0 through load-gate-run.sh and isolated-test-run.sh. This supplemental check did not repeat the historical 40-job suite or replace its counts. Its temporary worktree was clean and was removed by the owning script.

LOAD_GATE_SUMMARY threshold=15 waited_seconds=750 wait_timed_out=0 load_start=24.60 load_max=27.59 load_mean=21.73 samples=26 read_errors=0

`load_threshold=15 load_waited_seconds=750 load_wait_timed_out=0 load_start=24.60 load_max=27.59 load_mean=21.73 samples=26 read_errors=0`. No isolation TRIPWIRE.

This branch adds only the gate record after the reviewed source; it contains no implementation change after the test runs. The exact new PR head will be read back before deploy clearance and the merge-request. MPR owns the merge and must pin it to that cleared head; this record does not claim the change has landed.
