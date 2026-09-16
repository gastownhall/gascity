# Release gate: container grpc and Thrift security pins

- Deploy bead: `ga-kkktn4`
- Review bead: `ga-htipw0` — PASS
- Build bead: `ga-rl1l10`
- Reviewed source: `96b3ec3f450ed1883e27570244fb14a096541af0`
- Base: `origin/main@c6c81ec2822186f9cea8aff52d520450469a2bfc`
- Deploy branch: `deploy/ga-kkktn4-gate`
- Deploy mode: `remote`; push remote: `origin`
- Pre-flight: the reviewed source has no associated pull request
- Overall verdict: **PASS**

## Criteria

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-htipw0` is closed with verdict `pass` and pins exact source `96b3ec3f450ed1883e27570244fb14a096541af0`. It independently replayed the RED/GREEN sequence and reports no security, scope, or specification blocker. |
| 2 | Acceptance criteria met | **PASS** | `Dockerfile.base` forces grpc 1.83.2 into rebuilt gh and Dolt binaries; `Dockerfile.agent` forces grpc 1.83.2 and Thrift 0.24.0 into rebuilt bd and asserts both embedded versions. The grpc CVE waivers are removed, and the Thrift waiver is narrowed to the still-affected Dolt path. Fresh base, agent, and controller images built successfully; extracted bd module metadata contains both exact versions; pinned Trivy reports zero unwaived HIGH/CRITICAL findings for all three images and zero Dockerfile/Kubernetes misconfigurations. |
| 3 | Tests pass | **PASS with two attributed raw failures** | The isolated 40-job full local CI union completed **38 PASS / 2 raw FAIL / 0 omitted jobs**, comprising **47,189 PASS / 2 FAIL / 227 SKIP** top-level executions. Every diff-owned test passed in both selecting tiers: **8 PASS / 0 FAIL / 0 SKIP**. Both raw failures stopped during unrelated external beads database bootstrap, before their scenario assertions or candidate behavior, and are attributed below to a tracker that predates the run. The real container builds, artifact inspection, Trivy scans, build, vet, policy, affected-package lint, formatting, documentation, native dependency, core-boundary, event-isolation, and DoltLite-beads gates pass. `waiver_ref: none`; `ci_lane_run: n/a` because no CI configuration changed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-htipw0` records no blocker, major, unresolved HIGH, or security finding. The image-level Trivy enforcement scan is clean after the waiver removals, and each rebuilt binary's module floor is asserted during its Docker build. |
| 5 | Final branch is clean | **PASS** | Before this gate record was written, `git status --porcelain=v1`, `git diff --check origin/main...HEAD`, and the candidate formatting check were empty. The disposable container-build worktree was removed after use; all generated inputs and scan caches remain outside the repository. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 96b3ec3f450ed1883e27570244fb14a096541af0` exited 0 against `origin/main@c6c81ec2822186f9cea8aff52d520450469a2bfc` and produced tree `a29286f4c450b5e4283a1eb546c6e6fbb2213bb5`. No deployer self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Two reviewed TDD commits (RED then GREEN) change four files for one theme: move the rebuilt container tools above the grpc/Thrift security floors and remove only the corresponding obsolete waiver paths. The ancestry guard accepts deploy `ga-kkktn4`, review `ga-htipw0`, and build `ga-rl1l10`; no unrelated or `.claude/**` change is present. |

## Acceptance evidence

- `contrib/k8s/Dockerfile.base` raises `GRPC_VERSION` from `1.82.1` to `1.83.2`; the gh and Dolt build stages each assert that exact embedded module version.
- `contrib/k8s/Dockerfile.agent` adds `THRIFT_VERSION=0.24.0`, forces that version after the grpc override, and asserts both exact versions in `/out/bd`.
- `.trivyignore.yaml` removes `CVE-2026-84304` and `CVE-2026-84445` completely after the base-image grpc bump, and removes only `usr/local/bin/bd` from `CVE-2026-43871`; Dolt remains waived because it still pins Thrift 0.23.0.
- The rebuilt agent starts successfully and reports `bd version 1.3.0-rc.2 (c185735c38)`.
- Independent `go version -m` inspection of the extracted `/usr/local/bin/bd` reports `google.golang.org/grpc v1.83.2` and `github.com/apache/thrift v0.24.0`.
- Pinned Trivy 0.70.0 reports zero HIGH/CRITICAL findings for:
  - `gc-agent-base:latest` (`sha256:29450ae4fe316f33dfd850522112cdc3da80218d351944e4fa5f8ed67694e521`)
  - `gc-agent:latest` (`sha256:9951b9bcda6c8815c646f2194c389092ced50c4fd645253f0547ee340a9f0fd8`)
  - `gc-controller:latest` (`sha256:993fe35149d50171a8995b28b6f38a6950288c56a1384e44f8bafe6aaeb89f6d`)
- `trivy config` reports zero HIGH/CRITICAL misconfigurations across all 12 Dockerfile and Kubernetes configuration targets.
- Candidate scope is four files, 37 insertions, and 56 deletions. `go.mod` and `go.sum` are untouched.

## Criterion 3 evidence

- `test_cmd`: `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 CMD_GC_PROCESS_TOTAL=6 GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-kkktn4-gate-20260915T2010/jobs /home/jaword/projects/gc-management/packs/actual/all/scripts/isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- `test_cmd_scope: full-suite`
- `test_counts: 47,189 PASS / 2 raw FAIL / 227 SKIP`; job result `38 PASS / 2 raw FAIL / 0 omitted` out of 40.
- `all_execution_events: 79,844 PASS / 2 FAIL / 318 SKIP`, including subtests.
- `required_job_coverage`: the local union ran `unit-core`, all six local `cmd-gc-process` shards, `productmetrics-testhook`, all package-integration core/cmd-gc/tmux shards, review-formula basic/retry/recovery shards, bdstore, REST smoke, and all eight REST-full shards.
- `diff_tests_executed: 8 PASS / 0 FAIL / 0 SKIP` — each changed top-level test passed once in `unit-core` and once in `integration-packages-core-4-of-4`:
  - `TestContainerCLIToolsRebuildWithPatchedGRPC`
  - `TestAgentImageRebuildsBDAndGCWithPatchedGRPC`
  - `TestTrivyIgnoreDropsStdlibWaiversForRebuiltTools`
  - `TestTrivyIgnoreKeepsReviewedBridgeEntries`
- `skip_justification`: the 227 top-level skips are existing suite-controlled platform, capability, provider-matrix, root-only, helper-process, or opt-in exclusions. None is diff-owned, and every diff-owned test executed in both selecting tiers.
- `waiver_ref: none`
- `ci_lane_run: n/a (no workflow, job, matrix, timeout, or required-check configuration changed)`
- Per-job logs: `/var/tmp/ga-kkktn4-gate-20260915T2010/jobs`
- Per-job log manifest digest: `29334f4c91bb62a236b496b813a921ef0c38dbba1f9c56280528a00ead5a870e`
- Container-build and scan evidence: `/var/tmp/ga-kkktn4-container.J9PYxH`

### Criterion 3a failure attribution

The raw failures remain recorded as failures. Root-cause bead `ga-e2z1zb` existed before this run and tracks the exact random-cursor schema-bootstrap refusal. Each failure occurs in `test/integration`, while this candidate modifies Docker build inputs, a Trivy waiver file, and package-local static tests in `scripts`; there is no path overlap. The candidate changes neither beads initialization nor Dolt schema handling, and each fixture stopped before reaching its scenario assertions. This run's two exact sightings were appended to `ga-e2z1zb` and read back successfully.

| Raw failing test | Attribution |
|---|---|
| `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped when external bd treated a partially migrated `hq` database as empty and refused the resulting v53-to-v66 shared-server migration, before recovery logic or candidate container behavior. |
| `TestCleanInstallTutorialPath` | `failure_attribution: ... -> ga-e2z1zb | clause 3(a): MECHANISM — PASS`. Fixture `gc init` stopped on the tracked `hq` v56-to-v66 schema-bootstrap refusal before the tutorial assertions or candidate behavior. |

Condition tracker `ga-lejnse` identifies `ga-e2z1zb` as the reproduced root-cause/fix bead: concurrent initialization can expose a partially migrated database, which `op_init` misclassifies as empty before the shared-server migration safety check refuses the random intermediate cursor.

## Static, build, and policy evidence

- `make test-ci-policy` — PASS.
- `make check-gomod-replace` — PASS.
- `make check-native-dependency-surface` — PASS (`737` modules; `25` AWS, `9` Azure, `15` DoltHub, `1` Google API; `175,969,413` binary bytes).
- `make check-eventexport-isolation` — PASS.
- `make check-core-boundary` — PASS.
- `make test-native-doltlite-beads` — PASS.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=909879c882ececb519b380f050f1d467a8bb1171 make lint-affected` with a fresh per-run golangci cache — PASS, `lint-affected: ./scripts`, `0 issues`.
- `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=909879c882ececb519b380f050f1d467a8bb1171 make fmt-check-changed` — PASS.
- `make check-docs` — PASS.
- `go vet ./...` — PASS.
- `go build ./...` — PASS.
- `make check-hooks` — PASS; `.githooks` owns `core.hooksPath`.
- `git diff --check origin/main...HEAD` — PASS.
- Fresh disposable-worktree `make docker-base docker-agent` — PASS.
- `make docker-controller` against the rebuilt agent — PASS.
- Pinned Trivy 0.70.0 config and HIGH/CRITICAL enforcement scans for base, agent, and controller — PASS with zero findings.

## Environment integrity

- Rootless Podman was available through `/run/user/1000/podman/podman.sock`; Ryuk was disabled for the full test union as required on this host.
- Image builds and Trivy scans ran against the host Docker engine in a disposable worktree at the exact reviewed source SHA. The temporary Git worktree was removed after the build.
- The repository's isolated-test wrapper supplied the test environment and topology. The shared Go build cache was neither cleared nor redirected.
- No shared Dolt schema migration was attempted; the safety refusals were observed and attributed without mutating the shared server.
