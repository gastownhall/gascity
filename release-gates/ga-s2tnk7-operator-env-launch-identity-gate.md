# Release gate: operator-authored environment changes alter only Launch identity

- Deploy bead: `ga-s2tnk7`
- Build bead: `ga-i91hrn`
- Review bead: `ga-is95n2`
- Reviewed source: `4b8481da16ce49637211d0a8f6188379504a4dd4`
- Source merge base: `225211190684bc0f0aea52de4c49c03926f961bd`
- Base checked: `origin/main@8aeb003a790392fad319db395502dde43596d0e4`
- Deploy branch: `deploy/ga-is95n2-gate`
- Deploy mode: remote; push target determined at publication time
- Evaluated: 2026-09-12
- Verdict: **PASS with two attributed, non-diff-owned test failures and one attributed local lint condition**

## Gate checklist

Target pre-flight ran before criterion 6. The reviewed source resolves to the
full SHA above, has no existing pull request, and remains the exact commit that
received the review-of-record PASS. Criterion 6 passed against the freshly
fetched base before the remaining criteria were finalized.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review-of-record `ga-is95n2` is closed with verdict `pass` on exact source `4b8481da16ce49637211d0a8f6188379504a4dd4`. It records focused build, vet, lint, security, coverage, and scope evidence. |
| 2 | Acceptance criteria met | **PASS** | `runtime.Config.OperatorEnv` is a documented map separate from `Env`, `FingerprintExtra`, and the fixed internal environment allow-list. Its sorted-map input is included in Core and Launch fingerprints but excluded from Provision. Partition completeness tests classify it as Launch-tier. Tests prove nil/empty equivalence, stable map ordering, no caller-map mutation, and unchanged Provision identity. `FingerprintVersion` is `v6`; both golden surfaces and the version pin were rebaselined, and mismatch handling remains the silent rebaseline path. |
| 3 | Tests pass | **PASS with attributed failures** | The authoritative documented full-suite command completed all 40 jobs: **38 PASS / 2 raw FAIL / 0 SKIP jobs**. It recorded **46,618 PASS / 2 raw FAIL / 213 SKIP** top-level test events. Both failures are non-diff-owned and attributed under criterion 3a. Every one of the eight diff-owned tests ran and passed. `test_cmd_scope: full-suite`; `waiver_ref: none`. |
| 3a | Pre-existing failures attributed | **PASS** | `TestGraphWorkflowSuccessPath` and `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` are covered by pre-existing open tracker `ga-esyijp`. In both logs, fixture initialization invokes external `bd init`, which refuses pending schema migrations on the shared Dolt server before metadata is written and before the candidate runtime fingerprint code can execute. The candidate changes neither beads schema/bootstrap nor either failing integration test path. Both sightings were appended to and read against the tracker. |
| 3b | Policy and static lanes | **PASS with one attributed local condition** | `make build`, `make fmt-check vet`, `make test-ci-policy`, `make check-gomod-replace`, `make check-native-dependency-surface`, `make check-eventexport-isolation`, `make check-core-boundary`, and `make test-native-doltlite-beads` all passed. `git diff --check origin/main...HEAD` passed. `make lint` reported only two `govet` and one `revive` diagnostics in ignored, untracked dashboard `node_modules/flatted` Go code absent from `origin/main`; pre-existing open tracker `ga-bvixfw` covers that exact local condition, and the sighting was appended. |
| 3c | CI-config lane | **PASS — n/a** | `ci_lane_run: n/a (no CI workflow, required-check, job, matrix, or timeout change)`. |
| 4 | No high-severity review findings open | **PASS** | Unresolved HIGH findings: 0. The reviewer found no blocker or major issue; its one minor note is forward-looking guidance to treat `OperatorEnv` as potentially sensitive if runtime config is ever logged or serialized. |
| 5 | Final source is clean | **PASS** | The exact reviewed source was checked out detached and clean before creating this checklist. This gate file is the only deployer-authored change and will be committed separately on the isolated deploy branch. |
| 6 | Branch diverges cleanly from main | **PASS** | After a fresh fetch, `git merge-tree --write-tree origin/main 4b8481da16ce49637211d0a8f6188379504a4dd4` returned tree `8972821f37222012844156bf11652ae17de5b114` with exit 0 and no conflicts against `origin/main@8aeb003a790392fad319db395502dde43596d0e4`. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | Three source commits and nine files form one TDD feature theme: introduce a dedicated operator-authored environment identity input, classify it as Launch-tier, and rebaseline the shared fingerprint-version goldens. `assert_deploy_ancestry_scope` passed with deploy, review, and build beads `ga-s2tnk7`, `ga-is95n2`, and `ga-i91hrn`; no unrelated commit or `.claude/**` path rides in the range. |

## Full-suite test evidence

`test_cmd_scope: full-suite`

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock \
TESTCONTAINERS_RYUK_DISABLED=true \
EXTRA_TEST_ENV='DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true' \
GOFLAGS=-v \
GO_TEST_TIMEOUT=30m \
LOCAL_TEST_LOG_DIR=/var/tmp/ga-s2tnk7-full-verbose-20260912T2000Z \
make test-local-full-parallel
```

The rootless Podman socket was present and the repository-pinned Dolt 2.1.7
image was available before the run. All 40 documented jobs reached a terminal
state. Raw logs are retained at the path above and the runner transcript is at
`/var/tmp/ga-s2tnk7-full-verbose-20260912T2000Z.log`.

- `test_counts: 38 PASS jobs, 2 attributed raw FAIL jobs, 0 SKIP jobs`
- `top_level_test_events: 46,618 PASS, 2 attributed FAIL, 213 SKIP`
- `raw_test_failures: 2 attributed FAIL, none diff-owned`
- `diff_tests_executed: 8 PASS, 0 FAIL, 0 SKIP`
- `skip_justification: all 213 skips are explicit suite-controlled platform, helper-process, live-provider, update-golden, or opt-in persistence paths; no diff-owned test skipped`
- `waiver_ref: none`

Diff-owned results found in the authoritative verbose logs:

- `TestOperatorAuthoredEnvFingerprintIsDeterministicAndLaunchOnly` — PASS
- `TestOperatorAuthoredEnvFingerprintTreatsNilAndEmptyAsEquivalent` — PASS
- `TestFingerprintGolden` — PASS
- `TestFingerprintVersionPin` — PASS
- `TestFingerprintPartitionAccountsForEveryConfigField` — PASS
- `TestFingerprintPartitionCoversCoreDisjointly` — PASS, including the new `OperatorEnv` case
- `TestIsLegacyOrMismatchedVersion` — PASS
- `TestSessionCoreConfigForHashInfoGolden` — PASS

## Raw failures and attribution

| Raw result | Test | Tracker / proof |
|---|---|---|
| **FAIL — ATTRIBUTED** | `TestGraphWorkflowSuccessPath` | Open root-condition tracker `ga-esyijp`, created 2026-08-29. Clause 3(a) mechanism: external `bd init` refused 51 pending shared-server schema migrations (`v15 -> v66`) before writing `.beads/metadata.json`. The graph workflow never reached runtime fingerprint behavior. The diff changes neither `test/integration` nor the beads initialization/provider boundary. |
| **FAIL — ATTRIBUTED** | `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash` | Open root-condition tracker `ga-esyijp`, created 2026-08-29. Clause 3(a) mechanism: external `bd init` refused 12 pending shared-server schema migrations (`v54 -> v66`) before writing `.beads/metadata.json`. The recovery formula never began. The diff changes neither `test/integration` nor the beads initialization/provider boundary. |

`failure_attribution`:

- `TestGraphWorkflowSuccessPath -> ga-esyijp | clause 3(a) mechanism — tracked shared-server pending-migration refusal during fixture initialization; candidate runtime path unreachable`
- `TestRetryManagedPooledWorkerRecoversClaimedAttemptAfterCrash -> ga-esyijp | clause 3(a) mechanism — tracked shared-server pending-migration refusal during fixture initialization; candidate runtime path unreachable`

`inconclusive-guard: n/a — both attributions have decisive mechanism proof.`
The diff adds one runtime test file but changes no resource-census baseline or
suite target. Neither failure is a contention or resource-count failure.

## Policy-lane attribution

`policy_lane: build, fmt, vet, CI policy, module replacement, native dependency, event-export isolation, core boundary, and native DoltLite beads checks PASS; make lint raw FAIL attributed to ga-bvixfw`

`make lint` found two `govet` constant-inline diagnostics and one `revive`
package-comment diagnostic in
`internal/api/dashboardspa/web/node_modules/flatted/golang/pkg/flatted/flatted.go`.
The file is ignored, untracked, absent from `origin/main`, and outside the
candidate diff. Open tracker `ga-bvixfw` predates this run and names the exact
full-lint condition; the current sighting was appended.

## Pre-flight and ancestry evidence

- The reviewed SHA passed a hex-only guard and resolved to the full commit above.
- `gh api repos/gastownhall/gascity/commits/4b8481da16ce49637211d0a8f6188379504a4dd4/pulls` returned an empty list, so no already-merged or closed-PR reconciliation applies.
- `git rev-list --left-right --count origin/main...4b8481da16ce49637211d0a8f6188379504a4dd4` returned `5 3` at the recorded base.
- `assert_deploy_ancestry_scope origin/main 4b8481da16ce49637211d0a8f6188379504a4dd4 ga-s2tnk7 ga-is95n2 ga-i91hrn` passed. The accepted sibling is the build bead cited by all three source commits.

## Disposition

The gate is PASS. Publish the isolated deploy branch and pull request, attach a
successful `release-gate/deploy-clearance` status to the exact PR head, and send
the verified merge request to the mayor. Merge authority remains with the mayor
or merge processor.
