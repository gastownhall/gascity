# Fresh-session guard release gate — ga-omk7gr

**Verdict:** **PASS**

Evaluated 2026-09-29T01:57:09.204995+00:00. Remote mode; push remote `fork`. Reviewed source `7d01afef56222a454e6118703a41930e4632d37c`; review `ga-srhfkw` (PASS). Build lineage: `ga-l9ekh3`, `ga-2weagw`. Isolated target: `deploy/ga-omk7gr-gate`.

Base `a4ea8ee883ce05e680210ef64bed45e716986356` was `origin/main` when criterion 6 began. Canonical scratch merge `c80e4c9da68b465548a2ffa7837e9936ac44f957` has exactly that base and the reviewed source as parents. Tree `5037c64ee30e589efa1b8968824c686618d7ff04` equals the `git merge-tree --write-tree` result. Build, vet, policy and all test lanes below ran on this same merged tree. Main subsequently advanced; this record identifies the tested base explicitly.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Fresh `ga-srhfkw` review records `verdict: pass` and the resolved source SHA above. No carryover needed. |
| 2 | Acceptance criteria met | PASS | Regression file is registered in sorted Bazel gc_test srcs. Actual Bazel build/regeneration passed without tracked drift. Reviewed helper and test are preserved byte-for-byte. All three new regressions and all 14 guard/reassignment roots PASS in full process coverage. |
| 3 | Full CI-equivalent tests and policy | PASS | 40/40 full-suite jobs, required matched local CI lanes and policy succeeded; named executions, counts and skips below. |
| 4 | No unresolved HIGH findings | PASS | Review style/security/spec findings contain no unresolved HIGH finding. |
| 5 | Final branch clean | PASS | Source and canonical scratch checkouts are clean. The gate is the sole added artifact; post-commit clean readback is recorded on the bead before pushing. |
| 6 | Clean divergence from main | PASS | Canonical helper materialized the merge without conflict. `go build ./...` and `go vet ./...` both exited 0 on that tree. No self-rebase needed. |
| 7 | Single feature theme | PASS | Four source files implement and register regression coverage for previous-work fresh cycling. Full-range ancestry guard accepted only the confirmed lineage; no prohibited paths or unrelated feature. |

## Acceptance verification

The previous-bead lookup now uses the existing city/rig residency topology. Closed work falls back to its UpdatedAt when valid closed_at metadata is unavailable. Open rig work preserves the running session; real-close city and rig cases preserve an incarnation that started after closing. The caller uses only stores already held by this process. Independent comparisons against the earlier reviewed implementation confirmed the helper and regression file are byte-identical; the fresh review includes the sorted Bazel registration.

`bazel build //cmd/gc:gc_test` and `make bazel-sync` exited 0. Regeneration left all tracked files unchanged. `TestTutorial01` PASSed in process shard 6. All 14 guard/reassignment test roots independently PASSed; audit: `acceptance-guard-audit.json`.

## Full-suite evidence

- `test_cmd`: `load-gate-run.sh --threshold 15 --max-wait 1800 -- isolated-test-run.sh -- make test-local-full-parallel LOCAL_TEST_JOBS=4`.
- `test_cmd_scope: full-suite`; `GOFLAGS=-v`, `TMPDIR=/var/tmp`. The documented target includes fast baseline, six cmd/gc process shards, productmetrics, package/tmux/formula/bdstore/REST integration shards: 40 jobs, 40 PASS, 0 FAIL.
- Top-level emitted results: **54,070 PASS / 0 FAIL / 236 SKIP**. With subtests: **96,948 PASS / 0 FAIL / 331 SKIP**. These are execution-event counts across logs, including repeated executions, not distinct-test counts.
- `diff_tests_executed: 3 roots, six verified PASS executions; 0 FAIL / 0 SKIP`.
- Podman socket and exact library-pinned `dolthub/dolt-sql-server:2.2.0` image verified before running. `DOCKER_HOST`, `TESTCONTAINERS_RYUK_DISABLED=true`, `BEADS_ALLOW_UNREAPED_TESTCONTAINERS=1` supplied. CLI lanes use pinned bd 1.3.0 and Dolt 2.1.7; minimum-supported contract uses bd 1.0.4.
- `failure_attribution: none`; `policy_attribution: none`; `waiver_ref: none`.
- `skip_justification`: unmodified platform-only and opt-in live provider/credential cases, subprocess-only helpers, unit-tier process deferrals covered in later tiers, host subreaper limitations and optional external/live-fixture prerequisites. Original explanations and every skip event are retained in `skip-evidence.tsv` and shard logs. No diff-owned test skipped. Required topology tooling was supplied separately.

| Diff-owned test | Result | Full-suite log |
|---|---|---|
| `TestFreshCycleRepro_OpenPreviousBeadInRigStoreDefers` | PASS | `cmd-gc-process-2-of-6.log` |
| `TestFreshCycleRepro_IncarnationStartedAfterRealCloseDefers` | PASS | `cmd-gc-process-3-of-6.log` |
| `TestFreshCycleRepro_IncarnationStartedAfterRealCloseInRigStoreDefers` | PASS | `cmd-gc-process-4-of-6.log` |
| `TestFreshCycleRepro_IncarnationStartedAfterRealCloseInRigStoreDefers` | PASS | `integration-packages-cmd-gc-1-of-6.log` |
| `TestFreshCycleRepro_OpenPreviousBeadInRigStoreDefers` | PASS | `integration-packages-cmd-gc-5-of-6.log` |
| `TestFreshCycleRepro_IncarnationStartedAfterRealCloseDefers` | PASS | `integration-packages-cmd-gc-6-of-6.log` |

## Required CI coverage and policy

The `cmd/gc/session_*`/`cmd/gc/**` filters require worker phase1/phase2 and summaries, cmd/gc process/productmetrics, units/integration, beads topology and proxied-native acceptance. Unconditional static/preflight, acceptance A, minimum-supported bd contract, generated artifacts, release-config and dashboard lanes are included. Unrelated provider/pack filters and contract-acceptance-current do not match this diff. No workflow/config file changed: `ci_lane_run: n/a — no CI-config diff`.

`policy_lane`: `isolated-test-run.sh -- make GOLANGCI_LINT=<pinned-2.12.0> lint-affected fmt-check-changed test-ci-policy check-gomod-replace check-native-dependency-surface check-eventexport-isolation check-core-boundary test-native-doltlite-beads check-docs` — PASS. `LINT_CHANGED_SCOPE=tracked`, `LINT_CHANGED_REF` is the tested base; MAKEFLAGS carries the linter pin.

| Supplemental lane | Command result | Go emitted PASS / FAIL / SKIP events |
|---|---|---|
| worker-phase1-claude | PASS (exit 0) | 10 / 0 / 0 |
| worker-phase2-claude | PASS (exit 0) | 51 / 0 / 0 |
| worker-phase1-codex | PASS (exit 0) | 10 / 0 / 0 |
| worker-phase2-codex | PASS (exit 0) | 51 / 0 / 0 |
| worker-phase1-cursor | PASS (exit 0) | 8 / 0 / 2 |
| worker-phase2-cursor | PASS (exit 0) | 51 / 0 / 0 |
| worker-phase1-gemini | PASS (exit 0) | 8 / 0 / 2 |
| worker-phase2-gemini | PASS (exit 0) | 51 / 0 / 0 |
| worker-rollup-phase1 | PASS (exit 0) | 0 / 0 / 0 |
| worker-rollup-phase2 | PASS (exit 0) | 0 / 0 / 0 |
| acceptance-a | PASS (exit 0) | 396 / 0 / 15 |
| bd-cli-prev | PASS (exit 0) | 37 / 0 / 0 |
| topology-proxied | PASS (exit 0) | 23 / 0 / 0 |
| topology-shared-server | PASS (exit 0) | 1 / 0 / 0 |
| topology-migrate | PASS (exit 0) | 0 / 0 / 2 |
| topology-matrix | PASS (exit 0) | 11 / 0 / 1 |
| proxied-native | PASS (exit 0) | 17 / 0 / 0 |
| release-config | PASS (exit 0) | 0 / 0 / 0 |
| generated | PASS (exit 0) | 204 / 0 / 0 |
| dashboard-typecheck | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-test-types | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-e2e-types | PASS (exit 0) | 0 / 0 / 0 |
| generated-docs | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-vitest | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-fixture | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-browser | PASS (exit 0) | 0 / 0 / 0 |
| dashboard-render | PASS (exit 0) | 0 / 0 / 0 |

The legacy topology skips match the documented CI gap: `TestBeadsMigrateLegacyCityToProxied`, `TestBeadsMigrateProxiedRefusesLiveLegacyServer` and `TestBeadsInitTopologyMatrix/M5-legacy-gc-managed` need an unavailable pre-journal gc fixture. Proxied-default, shared-server isolation and M1 ran and PASSed. Native lifecycle/safety ran rather than skipping. Dashboard Vitest: 96 files / 932 tests PASS, 0 FAIL, 0 SKIP. Playwright render smoke: 19 PASS, 0 FAIL, 0 SKIP; retries=0, workers=1, fresh seeded server (CI=1).

The first supplemental topology invocation exited 2 in the shell before executing any test: Make consumed a regex dollar and left an unmatched quote. The original log is retained. Corrected recipes were syntax-validated and remaining lanes ran in separate continuation logs. This is a deployer invocation error, not a test failure, attribution, waiver or rerun-to-green.

## Host-load measurements

All wrappers measured five-minute load with threshold 15 and max wait 1800. Full metrics were also recorded on the bead.

```text
LOAD_GATE_SUMMARY threshold=15 waited_seconds=0 wait_timed_out=0 load_start=9.74 load_max=40.58 load_mean=25.02 samples=82 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=210 wait_timed_out=0 load_start=19.91 load_max=27.75 load_mean=20.70 samples=19 read_errors=0
LOAD_GATE_SUMMARY threshold=15 waited_seconds=300 wait_timed_out=0 load_start=24.64 load_max=24.64 load_mean=13.03 samples=38 read_errors=0
```

Artifacts: `/var/tmp/deploy-ga-omk7gr-r3.8ncfyqmh`. Full evidence: `suite-summary.json`, `gate-evidence.json`, `acceptance-guard-audit.json`, `skip-evidence.tsv`, build/vet/policy/Bazel logs, `required-lanes.log`, `required-continuation.log` and all shard/per-lane logs. Earlier failed gate remains on archive/ga-omk7gr-gate-r2.

Final pre-publication textual merge check: current origin/main 58022309bdb11d24930ca545e7aeca8d0208b184 also merges cleanly (exit 0). The full test evidence remains pinned to the base recorded above.
