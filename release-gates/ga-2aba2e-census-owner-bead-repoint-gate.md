# Release gate: census owner-bead re-point

- Deploy bead: `ga-2aba2e`
- Review bead: `ga-pv4gpg`
- Reviewed commit: `747ad85acbac64883258e5ce363e5cbaaaa8a8e2`
- Base: `origin/main@132ba4fc5729479978645b8fdad162e3c9aa9670`
- Deploy mode: `remote`; push target: `fork`
- Gate evaluated: 2026-09-11

`docs/PROJECT_MANIFEST.md` is absent in this repository. This checklist applies
the standard seven deploy criteria together with
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-pv4gpg` is closed with `verdict: pass` and pins deploy commit `747ad85acbac64883258e5ce363e5cbaaaa8a8e2`. The review records no blocker, major, minor, security, or uncovered-spec finding. |
| 2 | Acceptance criteria met | **PASS** | The target `ga-cp3hwi` is open and purpose-built as the durable resource-census owner. A word-level audit proves the four-file diff consists of exactly 143 old owner-ID tokens replaced by 143 `ga-cp3hwi` tokens: all nine requested legacy IDs are covered, including `ga-8pkpor`. `internal/testpolicy/resourcecensus/census.go` and `test/test-resources.toml` each contain zero `ga-80po0c` references at the reviewed commit. Numeric/resource baselines are byte-for-byte unchanged. The fail-closed ownership validator remains out of scope. |
| 3 | Tests pass | **PASS** | See the complete evidence and attribution below. The documented full-suite command ran on the exact reviewed commit with rootless Podman configured. It completed 36/40 jobs raw PASS; the four raw failures are attributed to two predating, independently evidenced conditions and are not caused by this diff. All modified tests reported PASS twice. |
| 4 | No high-severity review findings open | **PASS** | The reviewer recorded no blocker, major, minor, security, or other open finding; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --short --branch` was clean at detached reviewed HEAD before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | Pre-flight found no existing PR for the reviewed commit. `git merge-tree --write-tree origin/main 747ad85acbac64883258e5ce363e5cbaaaa8a8e2` exited 0 and produced tree `020b560ea2ac6b19118afcd74c973e778898e845`. The reviewed commit is 0 behind / 2 ahead of the current base, so no self-rebase was needed. |
| 7 | Single feature theme | **PASS** | Both commits perform one cohesive resource-census governance repair: update existing pinning assertions, then re-point synchronized owner identifiers across the code ledger, TOML ledger, and generated documentation table. |

## Criterion 3 evidence

`test_cmd`:

```text
DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GO_TEST_TIMEOUT=30m LOCAL_TEST_JOBS=4 GOFLAGS=-v make test-local-full-parallel
```

- `test_cmd_scope: full-suite`
- Rootless Podman 5.8.4 was live before the run.
- Repository-pinned `dolthub/dolt:2.1.7` and cached testcontainers/Dolt SQL
  Server images were present; Ryuk remained disabled as required.
- Runner jobs: **36 PASS / 4 raw FAIL / 0 SKIP**.
- Verbose result lines across the 40 job logs: **49,908 PASS / 4 raw FAIL /
  210 SKIP**.
- `skip_justification`: the skips are pre-existing opt-in, platform,
  privilege, provider-live, persistence, and helper exclusions. The diff adds
  no test, changes no skip condition, and none of the modified tests skipped.
- `ci_lane_run: n/a (no CI configuration, job, matrix, timeout, or required-check change)`.
- `waiver_ref: none`.

### Diff-owned tests

`internal/testpolicy/resourcecensus/census_test.go` modifies assertions inside
eight existing tests. Each reported PASS in both `unit-core` and
`integration-packages-core-1-of-4`: **16 PASS / 0 FAIL / 0 SKIP**.

- `TestValidateUsesCodeOwnedBootstrapPolicy` — PASS twice
- `TestBootstrapPolicyOwnsHTTPTestServerDebt` — PASS twice
- `TestBootstrapPolicyOwnsListenerHelperDebt` — PASS twice
- `TestBootstrapPolicyOwnsNetListenDebtAndExactMediumOwners` — PASS twice
- `TestBootstrapPolicyOwnsNetListenConfigDebt` — PASS twice
- `TestBootstrapPolicyOwnsNetListenPacketDebt` — PASS twice
- `TestBootstrapPolicyOwnsSyscallListenDebt` — PASS twice
- `TestBootstrapPolicyOwnsTmuxDebtAndExactMediumSetup` — PASS twice

### Raw failure attribution

All four failures satisfy the four attribution clauses: their test files are
outside the diff; their condition trackers predate this run and were opened;
this sighting was appended to and read back from each tracker; and there is no
path overlap or added test load. The diff changes only existing resource-owner
strings and existing string assertions, not numeric census baselines or suite
targets.

| Raw failing test | Tracker | Attribution proof |
|---|---|---|
| `TestPersonalWorkFormulaCompileAndRun` | `ga-esyijp` | Clause 3(a)/(b): fixture `gc init` stopped in external `bd init`, which refused pending shared-server schema migrations before formula execution. The candidate's changed census values are read by the `gc doctor` census-owner-liveness check, not by store initialization. The tracker contains the identical shared-server migration refusal on unrelated candidates before this run. |
| `TestAdoptPRFormulaRetriesTransientReviewerStep` | `ga-esyijp` | Same predating beads/schema-bootstrap condition and mechanism; the formula scenario never started. |
| `TestGraphWorkflowSuccessPath` | `ga-esyijp` | Same predating beads/schema-bootstrap condition and mechanism; graph execution never started. |
| `TestE2E_SuspendResume_City` | `ga-dc9utn` | Clause 3(a)/(d): exact 94.56-second missing `citysus.report` signature. The tracker independently reproduces and proves the named-session retirement defect in `build_desired_state.go` / `session_reconciler.go`; this four-file census change cannot execute that path. |

`failure_attribution: TestPersonalWorkFormulaCompileAndRun,
TestAdoptPRFormulaRetriesTransientReviewerStep, TestGraphWorkflowSuccessPath ->
ga-esyijp | clause 3(a)/(b) — external shared-schema bootstrap failure before
candidate behavior`

`failure_attribution: TestE2E_SuspendResume_City -> ga-dc9utn | clause
3(a)/(d) — independently reproduced and proven suspend/wake retirement defect`

### Policy and static lanes

- `policy_lane: make test-ci-policy — PASS`
- `go build ./...` — PASS
- `go vet ./...` — PASS
- `git diff --check origin/main...HEAD` — PASS
- `.githooks` is the configured hooks path.
