---
title: "Two-Minute CI With Blacksmith"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-04-29 |
| Author(s) | Codex |
| Issue | ga-nakct |
| Supersedes | N/A |

## Summary

Gas City's pull request CI currently returns its main signal in roughly
22-24 minutes for production PR runs. The dominant bottleneck is not runner
startup or dependency installation; it is coarse serial test grouping. The
longest PR lanes are `Integration / review-formulas` and
`Integration / rest`, each taking roughly 23 minutes in recent runs. `Check`,
`cmd/gc process suite`, and `Integration / packages` are secondary lanes in
the 8-10 minute range.

The Blacksmith partnership should be used to redesign CI around critical-path
latency rather than runner-minute efficiency. The target is a required PR
answer in two minutes for deterministic gates, but that target is a measured
Phase 4 SLO, not a Phase 1 promise. The design must first prove that runner
pickup, image verification, shard execution, artifact upload, and summary
fan-in can fit in a 120 second budget on real Blacksmith runners.

This design proposes:

1. A planner-driven CI graph that turns changed files and historical test
   timing into a matrix of small, independently scheduled jobs.
2. A trusted control-plane workflow for planner, manifest, and summary-gate
   logic, sourced from protected branch state rather than PR-modifiable code.
3. A prebuilt Gas City CI image so required jobs verify tool versions instead
   of installing Go, Node, Dolt, bd, tmux, jq, Claude CLI, and lint tools on
   every job.
4. Test-level sharding for integration, process-backed, and unit coverage
   lanes, with each shard sized for 45-75 seconds.
5. Stable branch-protection checks that aggregate high-fanout workers into
   a few human-readable required gates.
6. A phased migration path that first removes duplicate serial work, then
   introduces dynamic sharding, then hardens tests so the full deterministic
   suite can run on every PR.

## Context

### Current PR CI Evidence

Recent production PR CI runs show a stable critical path:

| Lane | Recent average |
|---|---:|
| `Integration / review-formulas` | ~23.1m |
| `Integration / rest` | ~22.6m |
| `Integration / packages` | ~9.8m |
| `cmd/gc process suite` | ~8.8m |
| `Check` | ~8.4m |

The last sampled real `ci.yml` PR runs clustered around a 23-24 minute wall
clock. A representative run was
<https://github.com/gastownhall/gascity/actions/runs/25097289892>.

The `Check` job serializes several independent gates:

| Step | Recent average |
|---|---:|
| `Lint` | ~3.35m |
| `Test` / `make test-cover` | ~2.0-2.3m |
| Tier A acceptance | ~1.7m |
| `Vet` | ~0.25m |
| dashboard drift check | ~0.25m |
| docs/spec checks | less than 0.1m each |

The repository already has useful shard boundaries:

- `scripts/test-integration-shard` can run `packages`,
  `review-formulas-basic`, `review-formulas-retries`,
  `review-formulas-recovery`, `bdstore`, `rest-smoke`, and `rest-full`.
- `.github/workflows/review-formulas.yml` already splits review-formulas into
  three parallel matrix jobs, but `.github/workflows/ci.yml` still runs the
  older sequential `make test-integration-review-formulas` lane.
- `Makefile` separates fast unit tests, `cmd/gc` process-backed tests,
  acceptance tiers, dashboard checks, OpenAPI generation, Docker, K8s, and
  provider-specific gates.

### External Platform Facts

Blacksmith runners are documented as drop-in replacements for GitHub-hosted
runner labels, with Linux x64 and ARM runners from 2 to 32 vCPU and no
Blacksmith-imposed concurrency limit. The same documentation describes
co-located cache behavior for official GitHub cache and setup actions.

Relevant sources:

- Blacksmith runner overview:
  <https://docs.blacksmith.sh/blacksmith-runners/overview>
- Blacksmith dependency cache:
  <https://docs.blacksmith.sh/blacksmith-caching/dependencies-actions>
- GitHub matrix job documentation:
  <https://docs.github.com/en/actions/how-tos/write-workflows/choose-what-workflows-do/run-job-variations>
- GitHub reusable workflow documentation:
  <https://docs.github.com/en/actions/how-tos/reuse-automations/reuse-workflows>

### Current Workflow Inventory

The migration must preserve the blocking intent of every current CI and RC
lane. This is the starting classification:

| Current lane | Current workflow | Target classification |
|---|---|---|
| `Check` | `ci.yml` | Required PR preflight, split into stable sublanes |
| `Release config` | `ci.yml` | Required PR preflight |
| `Dashboard SPA` | `ci.yml` | Required PR preflight |
| `cmd/gc process suite` | `ci.yml` | Required PR integration when path-gated today; full on infra changes, main, and RC |
| `Integration / packages` | `ci.yml` | Legacy lane may remain best-effort during overlap; replacement `CI / integration` lane is blocking with no `continue-on-error` |
| `Integration / rest` | `ci.yml` | Legacy lane may remain best-effort during overlap; replacement `CI / integration` lane is blocking with no `continue-on-error` |
| `Integration / bdstore` | `ci.yml` | Required provider integration when beads/Dolt paths change; full on main and RC |
| `Integration / review-formulas` | `ci.yml` and `review-formulas.yml` | Required when review-formulas paths or label match; remove duplicate sequential lane |
| `Worker core` and `Worker core phase 2` | `ci.yml` | Required when worker paths change |
| `Worker inference phase 3` | `ci.yml` | Catalog/report lane until executable inference scenarios land |
| `Pack compatibility gate` | `ci.yml` | Required when pack paths change |
| `MCP mail conformance` | `ci.yml` | Optional until upstream API drift is under local control |
| `Docker session` | `ci.yml` | Required when Docker-session paths change |
| `K8s session` | `ci.yml` | Optional unless K8s CI is configured |
| Tier B/C acceptance | `nightly.yml`, `rc-gate.yml` | RC-only and nightly, not two-minute PR path |
| Tutorial goldens | `rc-gate.yml` | RC-only |
| GoReleaser snapshot | `rc-gate.yml` | RC-only |
| macOS parity | `mac-regression.yml`, `rc-gate.yml` | Separate macOS gate with separate SLO |

### Two-Minute Latency Budget

The two-minute target is only accepted after a pilot proves this budget can
close. Phase 3 may target 2-4 minutes until these numbers are measured.

| Segment | Phase 4 budget |
|---|---:|
| Trusted planner workflow starts and emits manifests | 10s |
| Worker runner pickup plus checkout/image verification | 25s |
| Required shard execution p95 | 60s |
| Shard result and coverage artifact upload | 10s |
| Summary runner pickup and artifact fan-in | 10s |
| Summary validation and status publish | 5s |
| **Total** | **120s** |

Before Phase 3 replaces static sharding, run a Blacksmith pilot against
`integration-rest` with at least 32 simultaneous workers. The pilot must
publish:

- runner pickup p50/p95/p99 by runner label
- checkout and CI image verification time
- cache hit rate for Go, Node, and dashboard dependencies
- artifact upload and summary fan-in time at 32, 64, and 128 artifacts
- slowest-shard p50/p95/p99
- cost-per-PR estimate by runner label

If the measured budget cannot close, the public Phase 4 SLO changes before
the branch-protected graph changes.

## Problem

The current CI graph was built for scarce runner capacity. It keeps large
amounts of work inside a handful of jobs and Make targets. That shape is easy
to reason about, but it wastes the opportunity created by abundant compute:

1. The critical path is the longest coarse test group, not the amount of work.
2. Independent checks inside `Check` are serialized.
3. Some review-formulas work is duplicated between the main CI workflow and
   the dedicated review-formulas workflow.
4. Integration tests are grouped by historical convenience rather than
   measured runtime.
5. CI setup repeats installation and dependency hydration in every job.
6. There is no deterministic scheduler that can rebalance shards as the test
   suite changes.
7. Branch protection cannot require hundreds of volatile matrix job names
   directly; it needs stable summary checks.

The result is a ~23 minute PR loop. That latency is expensive even when runner
minutes are cheap because it slows agent and human iteration. Reducing the
required signal to two minutes would allow agents to run tighter fix loops,
merge smaller batches, and surface regressions while the author still has the
change in working memory.

## Goals

- Return the required deterministic PR signal in two minutes on warmed
  Blacksmith infrastructure.
- Keep branch protection stable as shard counts and names change.
- Run more total deterministic coverage on PRs than today, not less.
- Preserve full failure evidence from all shards instead of fail-fast hiding
  later failures.
- Make shard planning deterministic, inspectable, and reproducible locally.
- Keep CI behavior source-controlled and provider-portable where practical.
- Use Blacksmith's larger runners and concurrency without making all jobs
  unnecessarily expensive.
- Measure and continuously rebalance based on actual timing artifacts.

## Non-Goals

- Rewriting product logic or test semantics to make tests less meaningful.
- Treating nondeterministic inference, chaos, race, or long soak tests as
  required two-minute PR gates.
- Making branch protection depend on dynamic matrix job names.
- Adding hardcoded role or workflow behavior to Go production code.
- Using `pull_request_target` for untrusted code execution.
- Optimizing only for runner-minute cost. This design optimizes first for PR
  feedback latency.

## Design Principles

1. **Critical path over total work.**
   PR latency is the maximum lane duration after dependencies, not the sum of
   all lanes.
2. **Stable gates, dynamic workers.**
   Branch protection should require stable summary jobs while the internal
   shard graph can change as timing data changes.
3. **Planner output is an artifact.**
   Every run must publish the exact matrix the planner chose and why.
4. **Shard by measured runtime.**
   Historical Make targets are convenient entry points, but shard size should
   be driven by observed test duration.
5. **No hidden skips.**
   Every skipped lane needs a typed reason in the summary.
6. **Isolation before fanout.**
   Tests that share global state, ports, tmux sockets, Dolt state, or
   filesystem locations must be fixed before they enter high-concurrency
   shards.
7. **Provider abundance is not a correctness primitive.**
   The graph should run faster on Blacksmith, but correctness must not depend
   on proprietary CI behavior beyond documented runner labels and cache
   compatibility.

## Proposed Architecture

### CI Graph

The target PR graph has four layers:

```text
pull_request / push
  |
  |-- ci-plan
  |     produces preflight matrix, integration matrix, metadata, skip reasons
  |
  |-- preflight workers
  |     lint, fmt, vet, unit-cover shards, docs, spec, dashboard, acceptance-a
  |
  |-- integration workers
  |     packages[N], cmd-gc[N], rest[N], review-formulas[N], bdstore, providers
  |
  |-- summary gates
        CI / preflight
        CI / integration
        CI / required
```

`CI / required` is the branch-protected gate. It fails if any required
preflight or required integration shard fails, if any expected shard artifact
is missing, or if the planner itself fails.

### Trusted CI Control Plane

Planner, required-lane manifest, and summary-gate logic are control-plane
code. They must not be trusted from an unreviewed PR checkout.

The target layout is:

- `ci-control.yml`: reusable workflow invoked through `workflow_call` using
  the cross-repository form
  `gastownhall/gascity/.github/workflows/ci-control.yml@<protected-sha>`.
  PR workflows must not call `./.github/workflows/ci-control.yml`, because that
  resolves to the PR head version of the workflow.
- `ci-required-lanes.yaml`: minimum required-lane manifest protected by
  CODEOWNERS and loaded from the base branch for PRs.
- `scripts/ci-plan`: planner implementation used by the trusted workflow.
- `scripts/ci-summary`: summary implementation used by the trusted workflow.
- `scripts/ci-run-shard`: worker entry point. The worker command may execute
  PR code because it is the thing under test, but it cannot decide which
  required lanes exist or whether the run passed.

PR-modifiable worker artifacts are evidence, not authority. The summary gate
determines job conclusions from the GitHub Actions API and validates that each
expected artifact exists. It never accepts a worker-uploaded JSON field as
proof that a required check passed.

Before fork PRs are accepted for this repository, `CI / required` and
`RC / required` must be emitted by a workflow the PR cannot edit. Acceptable
patterns are an org/ruleset-managed required workflow or a minimal
`workflow_run` gate that consumes artifacts and GitHub Actions API state but
does not execute PR code. This fork-PR trust hardening is a Phase 3 acceptance
requirement, before dynamic planner output becomes authoritative for required
checks.

The required-lane manifest is a lower bound. The planner may add required
shards, but it may not omit a base-branch required lane unless the summary can
prove the lane is legitimately skipped by a typed policy rule.

Timing database writes are allowed only on protected branch runs. PRs may read
the latest protected timing snapshot and may write per-PR scratch timing
artifacts, but those scratch artifacts do not update the shared planner
database.

`ci-control.yml` generates the planner nonce and records it in
`expected_artifacts.json`. The same manifest records the verified
`gascity-ci@sha256:...` image digest. Workers echo these values into their
artifacts; the summary fails the run if they do not match.

If `ci-required-lanes.yaml` cannot be loaded, cannot be parsed, or contains
fewer than the minimum lane set, the trusted planner fails closed and
`CI / required` reports failure. A separate workflow hygiene test validates
the manifest syntax and minimum-lane set on every change to the manifest.

### GitHub Actions Topology Limits

The graph must stay inside GitHub Actions structural limits before the team
increases shard counts:

- Each matrix job must stay below 256 jobs.
- Dynamic matrix JSON must fit within GitHub's expression and output limits.
- Matrix job outputs are not used for shard accounting because reusable and
  matrix outputs collapse by last writer.
- Empty matrices are represented by one no-op row with `skip_reason`, or by a
  skipped matrix plus an unconditional summary that verifies the skip reason.
- Summary jobs run with `if: always()` and inspect `needs.*.result` plus the
  GitHub Actions jobs API so infrastructure failures, cancellations, and
  skipped matrices cannot pass silently.

Initial matrix partition:

| Matrix caller | Maximum Phase 4 rows | Notes |
|---|---:|---|
| `matrix-preflight` | 64 | lint, fmt, vet, docs, spec, dashboard, unit shards |
| `matrix-acceptance-a` | 32 | Tier A deterministic acceptance |
| `matrix-cmd-gc` | 64 | process-backed `cmd/gc` tests |
| `matrix-integration-packages` | 64 | package/test shards outside `test/integration` |
| `matrix-integration-rest` | 128 | `test/integration` rest shards |
| `matrix-review-formulas` | 64 | formula scenario shards |
| `matrix-provider` | 64 | bdstore, Docker, K8s, MCP mail, worker profiles |

If a matrix would exceed its cap, the planner must either increase runner size
and reduce shard count, split the suite into another matrix caller, or fail
the plan before any worker starts.

### Planner

Add `scripts/ci-plan` to emit JSON for each dynamic matrix. The trusted
workflow invokes the planner from protected branch code. Inputs:

- GitHub event name, PR draft state, labels, and changed files.
- Static lane definitions from protected `ci-required-lanes.yaml`.
- Historical timing data from the protected timing snapshot.
- Optional manual override inputs for RC and debugging workflows.
- A per-run nonce generated by the trusted workflow.

Outputs:

- `preflight_matrix.json`
- `integration_matrix.json`
- `optional_matrix.json`
- `planner_summary.md`
- `planner_decisions.json`
- `expected_artifacts.json`

Each matrix row has this shape:

```json
{
  "id": "integration-rest-07",
  "suite": "rest",
  "command": "scripts/ci-run-shard --suite rest --shard 7 --total 24",
  "runner": "blacksmith-8vcpu-ubuntu-2404",
  "timeout_minutes": 5,
  "required": true,
  "isolation_class": "process",
  "variant": "default",
  "coverage_required": true,
  "coverage_flag": "integration-rest",
  "expected_seconds_p75": 58,
  "expected_seconds_p95": 84,
  "skip_reason": "",
  "planner_nonce": "generated-by-trusted-workflow",
  "reason": "changed internal/session and cmd/gc paths"
}
```

The planner must be deterministic for the same inputs and timing database.
If timing data is missing, it falls back to conservative static shards checked
into the repo.

Allowed `isolation_class` values:

| Class | Meaning |
|---|---|
| `command` | non-Go command with no shared runtime state |
| `package` | Go package-level shard |
| `process` | process-backed test with isolated tmux/Dolt/home state |
| `subtest` | subtest-level shard proven safe by audit |
| `serial` | cannot share a runner process with another unit |

Allowed `skip_reason` values:

| Reason | Meaning |
|---|---|
| `path-gated` | skipped by protected path policy |
| `draft-pr` | skipped because the PR is draft |
| `label-required` | skipped until a force label is present |
| `oversized-deferred` | known non-required oversized unit deferred to nightly |
| `variance-oversized-deferred` | high-variance unit deferred until split or stabilized |
| `planner-fallback` | dynamic planning disabled and static fallback used |
| `dependency-failed` | upstream required setup failed |
| `not-configured` | provider lane lacks required repo secret/config |

### Timing Database

Every test-running shard writes timing data:

- Go package and top-level test durations from `go test -json`.
- Subtest durations where available.
- Command-level wall time for non-Go steps.
- Outcome, retry count, timeout, runner label, runner CPU count, runner CPU
  model when available, pickup time, commit SHA, workflow name, run ID, and
  run attempt.

Artifacts are merged only on protected branches into a compact timing
database. Phase 2 may collect timing into protected workflow artifacts and
GitHub Actions cache entries, but Phase 3 authority is a protected
`ci-metrics` branch in this repository. A post-main workflow merges successful
protected-branch timing artifacts, commits the compact snapshot to
`ci-metrics`, signs the commit with the CI GitHub App identity, and pushes only
from the protected workflow. PR planners fetch the latest `ci-metrics` commit,
record its SHA in `planner_decisions.json`, and treat it as read-only.

Cache-only timing data is never authoritative for dynamic planning.

Timing records include a stable identity:

```json
{
  "schema": 1,
  "unit_id": "test/integration:TestGraphWorkflowSuccessPath",
  "package": "github.com/gastownhall/gascity/test/integration",
  "test": "TestGraphWorkflowSuccessPath",
  "subtest": "",
  "variant": "default",
  "identity_aliases": [],
  "samples": 12,
  "duration_seconds_p50": 42,
  "duration_seconds_p75": 57,
  "duration_seconds_p95": 88,
  "last_success_sha": "abc1234"
}
```

The planner uses greedy bin packing by historical duration and variance:

1. Expand suite definitions to runnable units.
2. Assign each unit an expected duration using p75 once there are at least
   five successful samples; use static defaults before that.
3. Track p95 and mark units as variance hazards if p95 exceeds 90 seconds.
4. Sort longest first.
5. Place each unit into the currently shortest shard for that suite.
6. Repack only when predicted p95 improvement clears a configured hysteresis
   threshold, so minor timing noise does not reshuffle every plan.
7. Cap shards so expected p75 duration is 45-75 seconds for PR lanes, with a
   p95 maximum of 90 seconds.

Packing is tail-aware. A shard may accept a unit only if the sum of unit p95s
stays below the shard's p95 cap. Before a unit has enough samples for empirical
p95, the planner estimates p95 as `max(static_p95, 1.5 * p75)`. Empirical p75
requires at least 5 successful protected-branch samples. Empirical p95
requires at least 20 successful protected-branch samples. Retention pruning
must preserve enough samples for these thresholds or the planner falls back to
the conservative estimate.

If one runnable unit exceeds 90 seconds, it is marked `oversized` in the
summary and becomes required follow-up work to split the test itself.

Test identity aliases live in CODEOWNERS-protected `.github/ci/test-aliases.yaml`.
When a test is renamed, the implementation PR updates the alias file with
`old_unit_id -> new_unit_id`. The trusted planner loads aliases from the base
branch, resolves missing `unit_id` values through the alias map, and reports
unresolved renamed or deleted units in the planner summary.

### Runnable Units

The long-term runnable unit is:

- Go package for cheap packages.
- Top-level Go test for integration and process-backed suites.
- Subtest for any top-level test that exceeds 90 seconds and can be safely
  targeted with `-run`.
- Named non-Go command for dashboard, spec, lint, and release checks.

Initial suite mapping:

| Suite | Initial unit | Target |
|---|---|---|
| lint | command | one 32 vCPU lane |
| fmt | command | one small lane |
| vet | command or package shard | one or more lanes |
| unit-cover | package shard | 8-16 shards |
| docs | command | one small lane |
| spec | command | one small lane |
| dashboard | command group | one small lane |
| acceptance-a | top-level test | 2-4 shards |
| cmd-gc-process | top-level test | 8-16 shards |
| integration-packages | package/top-level test | 8-16 shards |
| integration-rest | top-level test/subtest | 16-32 shards |
| review-formulas | scenario/subtest | 8-16 shards |
| bdstore | top-level test | one lane until it grows |

### Runner Selection

Runner choice is part of the matrix row:

| Lane type | Default runner |
|---|---|
| tiny summary/docs/spec/fmt | `blacksmith-4vcpu-ubuntu-2404` |
| normal Go tests | `blacksmith-8vcpu-ubuntu-2404` |
| initial lint and package shards | `blacksmith-8vcpu-ubuntu-2404` |
| ARM parity lanes | `blacksmith-8vcpu-ubuntu-2404-arm` |
| macOS parity lanes | `blacksmith-12vcpu-macos-15`, outside the two-minute gate |

The gascity proof starts aggressively: tiny summaries can use 2-4 vCPU
runners, while heavyweight Linux lanes can move directly to 16 or 32 vCPU
runners. Later planner phases should record the speedup ratio in
`planner_decisions.json` so runner sizing can be tuned from measured data.
The timing artifact records requested runner label, `nproc`, CPU model,
pickup time, checkout time, and execution time.

The Phase 3 default hysteresis threshold is: repack only when the predicted
suite p95 improvement is at least 10 percent or 5 seconds, whichever is
larger. Implementation may tune this threshold only with a recorded
`planner_decisions.json` reason.

### Warm CI Image

Create a `gascity-ci` image or equivalent runner bootstrap layer containing:

- Go version from `go.mod` / workflow pin.
- Node 22.
- Dolt version from `deps.env`.
- bd release version from workflow env.
- `tmux`, `jq`, `curl`, `git`, `bash`, `python3`.
- `golangci-lint` pinned to `Makefile`.
- `oapi-codegen` pinned to `Makefile`.
- Claude CLI where required by deterministic test lanes.
- Dashboard dependency cache if compatible with the runner model.

The image has one canonical version manifest, `deps.env`, extended as needed
for Go, Node, `golangci-lint`, `oapi-codegen`, Dolt, bd, and Claude CLI pins.
Workflow steps verify versions and fail with actionable errors if the image
drifts.

The image supply chain is part of the required-path contract:

- Built by a dedicated protected workflow with `id-token: write`.
- Published to a registry whose write permissions are restricted to that
  workflow.
- Signed with cosign keyless signing.
- Consumed by digest (`gascity-ci@sha256:...`), never by mutable tag.
- Shipped with an SBOM generated by syft or an equivalent tool.
- Verified by signature and digest before required jobs run.
- Rebuilt on changes to `deps.env`, the image Dockerfile, workflow pins, or
  toolchain version files.
- Stored in an immutable registry path with retention long enough for PR
  reruns; image garbage collection may not delete a digest referenced by an
  active branch-protection run.
- Built and verified only with third-party actions pinned by commit SHA.

The cross-repository `ci-control.yml@<protected-sha>` reference is bumped only
by a CODEOWNERS-gated PR. That PR records the old SHA, new SHA, and workflow
hygiene result.

The tool-install fallback is restricted to `workflow_dispatch` on protected
branches and scheduled degraded-mode validation. It is not reachable from PR
triggers.

### Summary Gates

Dynamic worker names are not branch-protection API. Add stable summary jobs:

- `CI / preflight`
- `CI / integration`
- `CI / optional`
- `CI / required`

End state: only `CI / required` is branch-protected. `CI / preflight` and
`CI / integration` remain visible for diagnostics, but they are not protected
contexts after the migration overlap window.

`CI / required` runs unconditionally on every PR head SHA, with no path filter.
It reads the trusted planner manifest, GitHub Actions job status, and worker
artifacts. It verifies:

- Every expected required shard completed.
- Every required shard exited successfully.
- Every required shard uploaded timing and log metadata.
- Every expected coverage-producing shard uploaded coverage metadata.
- Every skipped lane has an explicit planner reason.
- No required lane is `continue-on-error`.
- Optional or experimental lane failures are visible but do not block unless
  configured as required.

Artifact transport is explicit:

- Each shard uploads exactly one result artifact named
  `ci-result-${planner_id}`.
- Each result artifact contains `result.json`, `timing.json`, and log excerpts.
- Coverage-producing shards also upload `coverage-${planner_id}`.
- Result artifacts include `GITHUB_RUN_ID`, `GITHUB_RUN_ATTEMPT`, planner ID,
  and the trusted planner nonce.
- The summary downloads artifacts with an explicit artifact pattern and
  validates one-to-one presence and uniqueness against `expected_artifacts.json`.
- Matrix outputs and reusable-workflow outputs are never used for shard
  accounting.
- Missing required artifacts fail the gate unless the run itself was
  superseded and cancelled.

The summary writes a first-failure-first table to `GITHUB_STEP_SUMMARY` and
uploads a machine-readable JSON report for agents.

### Failure Semantics

Use `fail-fast: false` for test matrices so one early failure does not hide
later failures. The summary job is responsible for failing the required gate.

Shard jobs should:

- Print the exact command being run.
- Emit `go test -json` where practical.
- Upload structured timing and result metadata.
- Upload failing logs or test artifacts.
- Avoid retries in the required path except for known external download/setup
  flakiness. Test retries hide product flakiness and belong in a separate
  flake-detection lane.

Cancelled superseded runs are not considered passing or failing required CI.
They produce a cancelled status. The newest uncancelled run for the PR head SHA
is the one branch protection evaluates.

### Failure UX

The summary must be useful at fanout scale. The top of `GITHUB_STEP_SUMMARY`
has this shape:

```text
CI / required: failed
3 of 142 required shards failed.
First failure: integration-rest-07 / TestGraphWorkflowSuccessPath
Local rerun:
  scripts/ci-run-shard --from-plan .ci/plan.json --id integration-rest-07
Container-parity rerun:
  docker run --rm -v "$PWD:/work" gascity-ci@sha256:<digest> \
    scripts/ci-run-shard --from-plan .ci/plan.json --id integration-rest-07
```

For each failed shard, the summary includes:

- suite, shard ID, runner label, actual duration, expected p75/p95
- first failing package/test/subtest
- last relevant log excerpt
- exact local rerun command
- link to full artifact

The human summary shows the first three failed shards inline. Additional
failures are collapsed behind `<details>` and fully represented in the JSON
artifact.

Each inline shard excerpt is capped at the last 50 relevant lines or 8 KiB,
whichever is smaller. `rerun_commands[]` entries have a typed shape:
`{"kind":"host"|"container","command":"..."}`.

The per-run plan is uploaded as `.ci/plan.json` and linked from the summary.
Agent consumers use the JSON summary rather than scraping GitHub UI. Phase 2
defines the versioned JSON summary schema before any agent integration depends
on it:

```json
{
  "schema": 1,
  "run_id": "github-run-id",
  "run_attempt": "github-run-attempt",
  "planner_mode": "static|dynamic|degraded",
  "planner_sha": "trusted-control-plane-sha",
  "timing_snapshot_sha": "ci-metrics-sha",
  "failed_shards": [],
  "skipped_shards": [],
  "oversized_units": [],
  "coverage_missing": [],
  "coverage_carried_forward": [],
  "rerun_commands": []
}
```

`scripts/ci-run-shard --from-plan` runs the same sanitize and isolation-lint
preconditions as CI when possible. The Docker command using
`gascity-ci@sha256:...` is the parity path; the host command is a convenience
path and is labeled as such in the summary.

### Coverage Architecture

Coverage is merged centrally. Individual shards never upload directly to
Codecov in Phase 3+.

Coverage flow:

1. The planner marks `coverage_required` on shards expected to produce
   coverage.
2. Each coverage shard uploads a raw coverage artifact named by planner ID.
3. `expected_artifacts.json` includes both raw shard coverage artifacts and
   the per-suite merged coverage artifact.
4. A per-suite coverage merge job validates the expected raw artifacts against
   the trusted manifest.
5. The merge job combines coverage deterministically. Binary coverage data
   uses `go tool covdata merge`; legacy text `-coverprofile` files use
   `scripts/merge-coverprofiles`, whose output is sorted by package/file/block
   so merge order does not affect the result.
6. `-race` coverage uses `variant: race` and uploads under a separate stable
   Codecov flag unless the suite explicitly opts into merging race coverage
   into the default baseline.
7. The merge job performs one Codecov upload per stable suite identity.
8. Missing required raw or merged coverage artifacts fail `CI / required`.

Failed shards do not contribute coverage. If a required coverage-producing
shard fails, the merge job records the missing contribution but does not merge
partial output from that shard into a green-looking profile.

Coverage baselines are separate:

| Baseline | Meaning |
|---|---|
| `required_pr_coverage` | always-run PR coverage only |
| `path_gated_pr_coverage` | PR coverage from lanes enabled by path policy |
| `full_deterministic_coverage` | full main/RC deterministic suite |

Carryforward is allowed only from protected branch coverage, only for
path-gated lanes, and only with a staleness bound: carryforward expires after
7 days or 50 protected-branch commits, whichever comes first. The summary must
surface carryforward age in both human and JSON reports. A failed required
shard may not carry forward stale coverage to appear green.

For path-gated PRs, Codecov status for `required_pr_coverage` is blocking.
`path_gated_pr_coverage` and `full_deterministic_coverage` are informational
unless the PR forced full CI. Full deterministic coverage is compared against
the latest protected `main` snapshot, with path-gated skipped suites shown as
not-run rather than zero-covered.

### Path Gating

Path gating remains useful, but it must be conservative:

- Always run preflight on every PR.
- Always run deterministic unit coverage on every PR.
- Run integration shards affected by changed paths.
- Run full integration on `main`, release candidates, and PRs that touch
  workflows, Makefile, test harnesses, provider boundaries, session lifecycle,
  beads, event bus, API schema, or shared internal packages.
- Allow labels such as `full-ci`, `needs-mac`, and `needs-review-formulas` to
  force lanes.

Every path-gated skip is reported in `planner_decisions.json`.

The force-full allowlist is CODEOWNERS-protected. Any change under these paths
disables PR path gating and runs the full deterministic Linux suite:

- `.github/workflows/**`
- `.github/actions/**`
- `.githooks/**`
- `Makefile`
- `TESTING.md`
- `deps.env`
- `scripts/ci-*`
- `scripts/test-integration-shard`
- CI image build files
- `internal/api/openapi.json`
- `docs/schema/openapi.*`
- `internal/api/genclient/**`
- `cmd/gc/dashboard/**`
- `internal/**`
- `test/**`

If a unit is `oversized-deferred` or `variance-oversized-deferred` for more
than 50 protected-branch commits, the post-main timing workflow opens or
updates a CI-debt bead with the unit identity and timing evidence.

### Nightly And RC Gates

With abundant compute, nightly should stop being the first place ordinary
deterministic regressions appear. Nightly becomes:

- repeated stress runs
- race detector sweeps
- chaos Dolt
- acceptance B/C
- synthetic inference
- macOS parity
- flake detection
- slow tutorial goldens

RC gate should reuse the planner and shard runner with a separate stable
summary, `RC / required`. RC policy disables PR path gating and forces all
deterministic lanes plus RC-only release checks:

- Tier B acceptance
- Tier C acceptance
- tutorial goldens
- GoReleaser snapshot
- macOS make test or macOS parity workflow
- release tag validation where applicable

## Test Harness Changes

### Isolation Requirements

Before a suite can enter high-concurrency PR fanout, its tests must satisfy:

- Unique temp directories via `t.TempDir()`.
- Unique `GC_HOME`, `GC_CITY`, and city names.
- Unique tmux sockets or guarded session prefixes.
- Unique Dolt directories and ports.
- No shared mutable global config without cleanup.
- No dependence on test order.
- No use of the default tmux server for cleanup.
- No sleeps where an event, process probe, HTTP health check, or file
  observation can provide a deterministic wait.

### Isolation Audit Gate

Phase 1 adds an isolation audit gate before high fanout. The first version can
be a script, `scripts/ci-isolation-lint`; it may later become a `go vet`
analyzer.

The gate fails on:

- `os.Setenv` in `_test.go` without `t.Setenv` or an audited process boundary.
- Hardcoded localhost ports outside an explicit allowlist.
- Tests that bind ports without requesting `127.0.0.1:0` or using the shared
  test port helper.
- `tmux` cleanup that does not specify an isolated `-L gc-test-<random>`
  socket.
- Any reference to the default tmux server in test cleanup.
- Writes outside `t.TempDir()`, a test-specific `GC_HOME`, or a test-specific
  repo temp root.
- References to `~/.dolt`, `~/.config/gc`, or host-global Gas City state in
  tests.
- Shared package globals mutated by tests without reset in `t.Cleanup`.

Every test shard runs `scripts/ci-runner-sanitize` before tests. It removes
only test-owned temp roots and test-owned tmux sockets; it never runs bare
`tmux kill-server` and never touches the default tmux server.

Phase 1 also produces an unsafe-test inventory:

| Field | Meaning |
|---|---|
| test identity | package/test/subtest |
| isolation violation | port, tmux, env, filesystem, Dolt, global state |
| current CI lane | where it runs today |
| required fix | concrete harness or test change |
| owner | responsible component/team |
| target phase | phase before which it must be fixed |

Tests not yet audited can still run in coarse static shards, but they cannot be
subtest-sharded or marked `t.Parallel()` until the inventory marks them safe.

### Parallelism In Go Tests

Add `t.Parallel()` only after isolation is proven. The goal is not to blanket
parallelize every test; it is to make each shard capable of consuming the
larger runner it requests.

Tests that cannot safely run concurrently must declare a serialized resource
class. The planner can still run many serialized-resource tests in separate
jobs if their resources are truly isolated per runner.

### Oversized Test Policy

Any required PR runnable unit with p50 over 90 seconds becomes a CI debt item.
The owning test should be split by:

- scenario table rows
- top-level test names
- subtest names
- setup fixture precomputation
- replacing fixed sleeps with event waits
- moving nondeterministic external behavior to nightly

The planner enforces p95, not only p50. A unit with p50 below 90 seconds but
p95 above 90 seconds is marked `variance-oversized` and must either be split,
made less variable, or excluded from the two-minute required path until fixed.

At least one required shard per high-risk process/integration suite should run
with `-race` once Phase 2 sharding makes that affordable. Race shards have a
separate expected-duration budget because they can run 2-3x slower.

## Workflow Changes

### Protected Check Migration

Branch protection changes are dual-published. No phase removes a currently
protected context until the replacement context has reported successfully on
the same PRs for an overlap window.

| Phase | Current context | Replacement context | Overlap | Rollback |
|---|---|---|---|---|
| 1 | `Check` | `CI / preflight` and `CI / required` | 10 successful PR runs | Continue emitting `Check` as an alias summary until ruleset edit lands |
| 1 | `Integration / review-formulas` sequential lane | split review-formulas plus `CI / integration` | 10 successful path-matched PR runs | Re-enable old Make target under the same summary name |
| 1 | `Integration / rest` | `Integration / rest-smoke`, `Integration / rest-full`, `CI / integration` | 10 successful path-matched PR runs | Collapse to old `make test-integration-rest` row |
| 2 | `cmd/gc process suite` | `cmd-gc[N]` plus `CI / integration` | 20 successful path-matched PR runs | Force one static shard running old target |
| 2 | `Integration / packages` | `packages[N]` plus `CI / integration` | 20 successful path-matched PR runs | Force one static shard running old target |
| 3 | static matrices | planner-generated matrices plus `CI / required` | 20 successful same-repo PR runs | Set `CI_PLANNER_MODE=static` and keep summary names |
| 4 | multiple visible summaries | `CI / required` as sole protected check | 20 successful non-draft PR runs after cache warmup | Keep `CI / required` but switch implementation to static fallback |
| RC | current `rc-gate.yml` job names | `RC / required` plus visible RC sub-summaries | 5 successful manual RC runs across two refs | Keep `RC / required` but switch implementation to current RC job graph |

Ruleset edits are made only after the overlap window and are recorded in the
implementation PR. Rollback must preserve the same protected check names; it
may change their implementation, but it must not require manual emergency
ruleset surgery to unblock merges.

Overlap windows have both run-count and event-coverage requirements. Before a
ruleset edit removes an old context, the overlap must include at least one
path-gated skip, one draft PR, one force-label PR, and one superseded/cancelled
run unless the context cannot observe that event type. The overlap window also
has a calendar floor of five business days. The old and new contexts must be
emitted by a single owning workflow on each SHA to avoid duplicate status
sources; the migration PR records that owner for every alias context. Branch
protection pins `CI / required` and `RC / required` to the GitHub Actions app
as the expected source.

### Phase 1: Remove Existing Waste

- Remove sequential review-formulas from `.github/workflows/ci.yml` or replace
  it with the split `review-formulas-basic`, `review-formulas-retries`, and
  `review-formulas-recovery` matrix.
- Split `Integration / rest` into `rest-smoke` and `rest-full`.
- Split `Check` into independent jobs with a stable `CI / preflight` summary.
- Add `concurrency` cancellation for superseded PR runs where missing.
- Switch gascity Linux and macOS workflow labels directly to Blacksmith for the
  proof window. No Windows lanes are in scope for gascity.
- Add `scripts/ci-isolation-lint` in report-only mode and publish the
  unsafe-test inventory.
- Add `ci-required-lanes.yaml` with the current lane inventory and protected
  skip policy.
- During overlap, legacy contexts may retain their current `continue-on-error`
  behavior, but the new `CI / preflight`, `CI / integration`, and
  `CI / required` contexts contain no `continue-on-error` on required lanes.

Expected PR critical path after Phase 1: 10-15 minutes.

### Phase 2: Static High-Fanout Shards

- Add static package sharding for `unit-cover`, `integration-packages`, and
  `cmd-gc-process`.
- Add one job per current review-formulas scenario.
- Add one job per current rest top-level test group.
- Add summary gates and artifact validation.
- Start collecting timing artifacts.
- Add coverage artifacts and suite-level merge jobs, but keep Codecov upload
  volume conservative until the merge path is proven.
- Run the Blacksmith pilot and publish the latency budget measurements.

Expected PR critical path after Phase 2: 5-8 minutes if no individual test
remains oversized.

### Phase 3: Dynamic Planner

- Implement `scripts/ci-plan`.
- Implement `scripts/ci-run-shard`.
- Replace static matrices with planner-generated matrices.
- Store timing artifacts and rebalance shards automatically.
- Add local reproduction command:

```bash
scripts/ci-run-shard --from-plan .ci/plan.json --id integration-rest-07
```

Phase 3 cannot become the default until the trust model, artifact contract,
coverage merge, timing database authority, and branch-protection migration
have all landed.

Expected PR critical path after Phase 3: 2-4 minutes, bounded by the longest
individual test and runner pickup time.

### Phase 4: Two-Minute Hardening

- Split or rewrite every oversized required test unit.
- Add warmed CI image verification.
- Tune runner sizes from measurements.
- Move long nondeterministic lanes to nightly or optional required-on-label
  workflows.
- Make `CI / required` the sole branch-protected aggregate gate for CI.
- Keep macOS and ARM parity outside the two-minute gate unless they have a
  separately measured SLO.

Expected PR critical path after Phase 4: approximately two minutes on warmed
Blacksmith infrastructure.

### Degraded Mode

Blacksmith outage or severe queue degradation must not make CI structurally
wrong. Degraded mode sets `CI_RUNNER_PROFILE=github-static`:

- runner labels switch to `ubuntu-latest`
- warm-image assumptions are disabled
- planner collapses fanout to Phase 2 static shards
- GitHub cache is used instead of Blacksmith colocated cache
- `CI / required` and other protected summary names remain unchanged

Expected degraded critical path is 8-15 minutes, not two minutes. The fallback
workflow is validated on a schedule so it does not rot.

During a Blacksmith incident, maintainers listed in CODEOWNERS for
`.github/workflows/**` are authorized to flip `CI_PLANNER_MODE=static`.
The incident PR or workflow dispatch must record the reason and the expected
rollback trigger.

## Observability

Every run must expose:

- Planner JSON and human summary.
- Per-shard command, runner label, expected duration, actual duration, and
  result.
- Per-test timing data where available.
- Coverage artifacts with stable flags.
- Oversized-test report.
- Skipped-lane report with reasons.
- A trend summary comparing p50/p95 PR latency over the last 10, 50, and 100
  runs.
- First-failure-first summary with exact local and container-parity rerun
  commands.
- Coverage merge report listing expected, present, missing, and carried-forward
  coverage artifacts.
- Runner pickup, checkout, image verification, test execution, artifact upload,
  and summary fan-in timings as separate fields.

## Security

The design keeps current PR trust boundaries:

- No untrusted PR code runs under `pull_request_target`.
- Secrets are not exposed to forked PR code.
- Uploads from PRs are limited to artifacts and coverage paths already deemed
  safe.
- Planner, required-lane manifest, and summary logic execute from protected
  branch state, not PR-modifiable code.
- Summary jobs consume only artifacts from the same workflow run and validate
  each artifact against the trusted manifest, run ID, run attempt, and planner
  nonce.
- Shared timing-database writes are restricted to protected branch runs.
- The CI image is signed, digest-pinned, SBOM-backed, and verified in required
  jobs.
- All third-party actions in required workflows are pinned by commit SHA with
  a version comment.

If Blacksmith runner labels are configured at the organization level, the
repository must ensure the Blacksmith GitHub App is installed for this repo
before switching labels. Blacksmith documents that jobs can queue if runner
labels are used in repositories not visible to the app.

Add a required workflow hygiene check:

- fail if third-party actions use mutable tags instead of SHAs
- fail if required-path jobs use `continue-on-error`
- fail if `pull_request_target` checks out or executes PR code
- fail if CODEOWNERS does not cover `.github/workflows/**`, `.github/actions/**`,
  `scripts/ci-*`, `deps.env`, and CI image build files

## Rollback

Each phase must be independently revertible:

- Phase 1 can fall back to existing Make targets.
- Phase 2 static shards can be disabled by forcing a single matrix row per
  suite.
- Phase 3 planner can fall back to a checked-in static plan.
- Runner labels can revert from `blacksmith-*` to `ubuntu-latest` through a
  single workflow env change.
- Degraded mode can collapse fanout to static shards without changing
  protected check names.

The old Make targets remain during migration so developers and release
operators have a known-good escape hatch.

## Acceptance Criteria

Phase 1 is accepted when:

- Main PR CI no longer runs sequential review-formulas in both places.
- `Check` is split into independent preflight jobs with a stable summary.
- Rest smoke and rest full are separate lanes.
- Branch protection dual-publishes old and new stable summary checks.
- `ci-required-lanes.yaml` inventories current PR/RC lanes.
- Isolation audit runs in report-only mode and publishes unsafe-test inventory.
- `ci-required-lanes.yaml` has a fail-closed parser test and minimum-lane-set
  test.
- The trusted reusable-workflow invocation uses the SHA-pinned cross-repository
  form, not a PR-local `./.github/workflows/...` reference.
- The shared test port/Dolt helper exists before the isolation lint rejects
  hardcoded ports.

Phase 2 is accepted when:

- `unit-cover`, `integration-packages`, and `cmd-gc-process` have static
  shards.
- Required test workers upload timing artifacts.
- Summary gates validate expected artifacts.
- Coverage-producing shards upload raw coverage artifacts and suite-level
  merge jobs validate expected artifacts.
- Blacksmith pilot measurements publish the latency budget table.
- No required-path job uses `continue-on-error`.
- The versioned JSON summary schema exists and includes failed shards, skipped
  shards, oversized units, coverage missing, carryforward, and rerun commands.
- Coverage merge artifacts are themselves included in `expected_artifacts.json`.

Phase 3 is accepted when:

- `scripts/ci-plan` produces deterministic matrix JSON.
- `scripts/ci-run-shard` can reproduce any shard locally from a saved plan.
- The planner uses historical timings and reports fallback behavior.
- CI publishes oversized-test and skipped-lane reports.
- Planner, manifest, and summary logic execute from trusted protected branch
  state.
- Shared timing writes are protected-branch-only.
- Summary checks validate artifact nonce, run ID, run attempt, uniqueness, and
  GitHub Actions API job conclusions.
- Timing database authority is the protected `ci-metrics` branch, not
  cache-only.
- The protected `ci-metrics` branch exists and planner runs record the timing
  snapshot SHA.
- `.github/ci/test-aliases.yaml` exists and is loaded by the trusted planner.
- Fork-PR trust hardening is implemented before dynamic planner output is
  authoritative for required checks.

Phase 4 is accepted when:

- The p50 required PR signal is at or below two minutes for 20 consecutive
  non-draft same-repo PR runs after cache warmup.
- The p95 required PR signal is at or below four minutes over the same window.
- No deterministic required suite is covered only by nightly.
- All required shards have p50 under 90 seconds.
- All required shards have p95 under 90 seconds or are explicitly split before
  becoming required.
- `CI / required` is the sole branch-protected CI summary after the overlap
  window, with static fallback preserving the same context name.

## Risks

### Flaky Tests Hidden By Fanout

Fanout can make flakes more visible and harder to ignore. Required shards
should not auto-retry product tests. Instead, flake data should be collected
and exposed so owners can fix the root cause.

### Artifact Fan-In Complexity

Hundreds of jobs create many artifacts. The summary gate must validate expected
artifacts from the planner rather than globbing blindly.

Mitigation: each shard uploads one result artifact and, if applicable, one
coverage artifact named by trusted planner ID. The summary validates exact
presence, uniqueness, nonce, run ID, and run attempt. Fan-in time is measured
and included in the two-minute latency budget.

### Cost Blowup

Money is not the first constraint for this design, but runaway cost can still
hide design mistakes. Every job records runner label and duration so cost per
lane can be estimated even if it is not the primary optimization.

### Warm Image Drift

A prebuilt image can make failures surprising if it silently drifts. Version
verification must be explicit and early in every job.

Mitigation: digest pinning, cosign verification, SBOM publication, and a single
version manifest make drift detectable before tests run.

### Overfitting To Blacksmith

The CI should benefit from Blacksmith but not require proprietary APIs for
correctness. Runner labels and cache behavior are enough for the first
implementation.

## Settled Implementation Choices

1. The initial gascity proof uses Blacksmith Linux labels from 2 to 32 vCPU.
2. macOS parity moves to `blacksmith-12vcpu-macos-15` as part of the proof.
3. gascity has no Windows CI scope for this proof.
4. There is no cost ceiling during the proof window; size right after timing
   data exists.
