# Release gate: agent suspend/resume comment preservation

- Deploy bead: `ga-we6ffm`
- Build bead: `ga-gc16k3` (round-three fixup: `ga-47m118`)
- Review bead: `ga-288fgv`
- Reviewed source: `8ea15a2cc0d23ee3d1649613bb8030c984ab2766`
- Base evaluated: `origin/main@202c53604dbd8961f81a2d0031ff904478c5a758`
- Deploy mode: `remote`; push remote: `fork`
- Gate result: **PASS with attributed raw test failures**

`docs/PROJECT_MANIFEST.md` is absent from this repository and reviewed
revision. This checklist applies the seven criteria in the deployer contract,
the implementation bead's acceptance contract, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review `ga-288fgv` is closed with `verdict: pass`, `tests_green: true`, no style/security finding, and `deploy_commit: 8ea15a2cc0d23ee3d1649613bb8030c984ab2766`. The SHA independently resolves to that exact commit. No review carryover is used. |
| 2 | Acceptance criteria met | **PASS** | Agent suspend/resume now changes only the `suspended` key through surgical TOML edits, preserving comments, absent tables, array formatting, and surrounding bytes for inline and pack-derived declarations. Resume removes the key rather than materializing a false value. An ambiguous legacy patch block now returns a wrapped `ErrSurgicalAgentEditUnsupported` instead of falling through to a lossy full rewrite. All ten diff-owned tests passed twice in the full suite. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented full local CI union completed all 40 jobs: **34 jobs PASS / 6 jobs with raw failures / 0 omitted**. Its top-level result lines contain **45,305 PASS / 7 raw FAIL / 191 SKIP**. Every one of the ten diff-owned tests passed in both unit and integration-package lanes, with 0 diff-owned FAIL and 0 diff-owned SKIP. All seven raw failures are non-diff-owned, tracked, mechanism-attributed, and path-disjoint under criterion 3a. Required policy and vet lanes passed. |
| 3a | Non-diff-owned failures attributed | **PASS** | Five verified open trackers cover the complete failure set: installed-`bd` manifest drift (`ga-f0uceo`), expired provider-catalog waivers (`ga-cojd80`), host tmux default bindings (`ga-k3fxvj`), circuit-breaker diagnostics contaminating machine-readable `bd` stdout (`ga-io7xwr`), and an external Dolt SQL server failing readiness (`ga-woq0zj`). The first four trackers predate the run. The Dolt readiness tracker was filed and verified during this discovering run under the landed-mechanism exception: the connection refusal occurs entirely inside an unchanged external-server fixture, with clauses 1 and 4 clear. Sightings were appended and read back from every tracker. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passed. `go vet ./...` passed. `git diff --check origin/main...8ea15a2c...` passed. |
| 3c | CI-config lane run | **PASS / n/a** | The candidate changes no workflow, CI job, matrix, timeout, required-check list, Makefile, or runner policy. `ci_lane_run: n/a (no CI-config change)`. |
| 4 | No high-severity review findings open | **PASS** | Review `ga-288fgv` records no unresolved HIGH finding. Its security analysis confirms the change is error propagation and byte-preserving local configuration mutation only, with no new input, authority, dependency, endpoint, serialization format, or sensitive-data surface. |
| 5 | Final branch clean | **PASS** | The exact-candidate detached worktree remained clean after the full suite, policy lane, vet, and diff check. All logs are outside the worktree. This checklist is the deployer's only new file and will be committed on the isolated branch. |
| 6 | Branch diverges cleanly from main | **PASS** | A fresh commit-to-PR lookup returned no PR. After the final fetch, `git merge-tree --write-tree --messages origin/main 8ea15a2c...` exited 0 and produced synthetic tree `bd40fc2896506d8db38788d3da08cbe867201510`, with clean auto-merges of `cmd/gc/cmd_agent.go` and `internal/configedit/configedit.go`. `assert_deploy_ancestry_scope` passed for the deploy/review/build lineage, with no `.claude/**` path or unrelated commit. No self-rebase was required. |
| 7 | Single feature theme | **PASS** | Six TDD/fixup commits change five files for one theme: safe, byte-preserving agent suspend/resume and explicit refusal when a legacy patch cannot be edited unambiguously. |

## Acceptance evidence

- Inline-agent suspend and resume preserve header, section, and inline comments
  byte-for-byte outside the target key.
- Absent TOML tables remain absent and existing inline arrays are not reflowed.
- Pack-derived and locally discovered agents use surgical patch edits.
- Resume deletes `suspended` rather than writing `suspended = false`.
- A missing or duplicate legacy patch block fails without changing the file.
- Error propagation reaches the CLI/API caller instead of triggering the old
  full-struct write fallback.
- The feature diff is confined to `cmd/gc/cmd_agent.go`,
  `internal/config/site_binding_agent_suspend.go` and its test, and
  `internal/configedit/configedit.go` and its test.

`diff_tests_executed` (each recorded PASS twice, with 0 FAIL/SKIP):

- `TestWriteCityAgentSuspendedForEdit_PreservesCommentsAcrossSuspendResume`
- `TestWriteCityAgentSuspendedForEdit_DoesNotMaterializeAbsentTableOrReflowArray`
- `TestWriteCityAgentSuspendedForEdit_DiffLimitedToSuspendedKey`
- `TestWriteCityAgentSuspendedForEdit_ResumeDeletesSuspendedKeyEntirely`
- `TestWriteCityAgentSuspendedForEdit_RefusesWhenAgentBlockNotFound`
- `TestSuspendAgent_Inline_PreservesComments`
- `TestResumeAgent_Inline_PreservesComments`
- `TestSuspendAgent_PackDerived_PreservesComments`
- `TestResumeAgent_LocalDiscovered_StripsLegacyPatchSuspended_PreservesComments`
- `TestResumeAgent_LocalDiscovered_AmbiguousLegacyPatch_RefusesLossyRewrite`

## Test evidence

The container-backed environment was established before the independent run:

- rootless Podman 5.8.4 was reachable at
  `unix:///run/user/1000/podman/podman.sock`;
- `TESTCONTAINERS_RYUK_DISABLED=true` was set as required on this host;
- cached Dolt SQL Server 1.32.4 was present;
- the candidate does not introduce or change a container-image pin.

```text
test_cmd: DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true GOFLAGS=-v LOCAL_TEST_LOG_DIR=/var/tmp/ga-we6ffm-full.y7Vkai make test-local-full-parallel
test_cmd_scope: full-suite
test_job_counts: 34 PASS / 6 raw FAIL / 0 omitted
test_counts: 45,305 PASS / 7 raw FAIL / 191 SKIP
diff_tests_executed: 10 unique tests, each PASS in unit and integration-package lanes; 0 FAIL; 0 SKIP
skip_justification: all 191 skips are unchanged platform, permission, optional-provider, live-infrastructure, helper-process, or opt-in integration guards; no diff-owned test skipped
waiver_ref: none
ci_lane_run: n/a (no CI configuration change)
runner_log: /var/tmp/ga-we6ffm-artifacts.9aMjz7/full-suite.log
shard_logs: /var/tmp/ga-we6ffm-full.y7Vkai
policy_log: /var/tmp/ga-we6ffm-artifacts.9aMjz7/policy.log
vet_log: /var/tmp/ga-we6ffm-artifacts.9aMjz7/vet.log
```

The command scheduled the complete 40-job local CI union, including every
non-short `cmd-gc-process` shard, every integration `cmd/gc` package shard,
unit/core, runtime/tmux, bdstore, review-formula, REST-smoke, and REST-full
lanes. All six process-backed `cmd/gc` shards and all six integration `cmd/gc`
package shards passed, satisfying the required-job mapping for the changed
`cmd/gc/**` and `internal/**` paths.

### Raw failures and attribution

| Raw result | Tracker | Four-clause evidence |
|---|---|---|
| `TestBdFlagManifestCurrent` | `ga-f0uceo` | The unchanged test compares the externally installed `bd --help` surface with `internal/bdflags`. The candidate changes neither side, the open tracker predates this run, and there is no path overlap. |
| `TestCatalogMatchesProductionWiringAndDocumentation` (2 occurrences) | `ga-cojd80` | Eight `runtime.Provider` waiver entries expired on 2026-08-26. This date-driven catalog-policy condition cannot be caused by the suspend/resume diff, its tracker predates the run, and there is no path overlap. |
| `TestGetKeyBinding_CapturesDefaultBinding`, `TestGetKeyBinding_CapturesDefaultBindingWithArgs` | `ga-k3fxvj` | Both unchanged tests read empty default bindings from this host's tmux server. The candidate changes no runtime/tmux path, and the exact open tracker predates this run. |
| `TestCleanInstallTutorialPath` | `ga-io7xwr` | The external `bd config get issue_prefix` command emitted a circuit-breaker cleanup diagnostic onto stdout before the correct `tra` value. The candidate cannot change the external `bd` output channel; the exact tracker predates this run and the test file is outside the diff. |
| `TestCompactScriptRealDoltRemotePush` | `ga-woq0zj` | The unchanged fixture's external Dolt SQL server never became query-ready within 20 seconds and the final client attempt received connection refused before the compaction script ran. The candidate changes no Dolt startup/compaction path. The landed mechanism plus clear clauses 1/4 permits the verified same-run tracker. |

Every raw failure satisfies: (i) its test is not diff-owned; (ii) the named
open tracker covers the root condition; (iii) the external/date-driven
mechanism proves the candidate did not cause it; and (iv) the failing test path
does not overlap the five-file diff. Because the mechanism proofs land, the
inconclusive reachability guard is not invoked. The candidate adds ordinary
tests inside already tracked packages but no suite target, matrix entry,
parallelism, resource-census change, server, or listener.

```text
failure_attribution: TestBdFlagManifestCurrent -> ga-f0uceo | clause 3(a) mechanism — installed-bd manifest drift; no path overlap
failure_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | clause 3(a) mechanism — date-expired provider waivers; no path overlap
failure_attribution: default tmux binding tests -> ga-k3fxvj | clause 3(a) mechanism — host tmux binding state; no path overlap
failure_attribution: TestCleanInstallTutorialPath -> ga-io7xwr | clause 3(a) mechanism — external bd diagnostic contaminates stdout; no path overlap
failure_attribution: TestCompactScriptRealDoltRemotePush -> ga-woq0zj | clause 3(a) mechanism — external server fails readiness before compact script; no path overlap
inconclusive-guard: n/a — all mechanism proofs landed; added_test_load=no
```

## Policy and static evidence

- `policy_lane: make test-ci-policy — PASS`
- `static_lane: go vet ./... — PASS`
- `format_lane: git diff --check origin/main...8ea15a2c... — PASS`
- `ci_lane_run: n/a (no CI configuration change)`

## Pre-push revalidation

The normal push ran the repository's `test-fast-parallel` hook. Nine of ten
jobs passed, including all six `cmd/gc` shards, the concurrency/lock
self-tests, and the Darwin compile check. `unit-core` reproduced only the
same `TestCatalogMatchesProductionWiringAndDocumentation` condition: eight
`runtime.Provider` waivers owned by `ga-80po0c.3` expired on 2026-08-26.
That deterministic, non-diff-owned failure remains covered by open tracker
`ga-cojd80`; the exact pre-push sighting was appended and read back. Under
the shared non-diff-owned gate-failure protocol this attributed hook failure
does not invalidate the PASS gate and permits publishing with hook bypass.

```text
pre_push_gate: 9 PASS / 1 attributed FAIL
pre_push_attribution: TestCatalogMatchesProductionWiringAndDocumentation -> ga-cojd80 | date-expired runtime.Provider waivers; no path overlap
pre_push_logs: /var/tmp/gc-local-tests.lFvtSD
```

## Disposition

All seven release criteria pass with complete non-diff attribution. Cut
`deploy/ga-we6ffm-gate` from exact reviewed source
`8ea15a2cc0d23ee3d1649613bb8030c984ab2766`, commit this checklist there,
push the isolated branch, and open the pull request. The description's stale
`deploy/ga-288fgv-gate` suggestion is provenance text only and is not used.
Merge authority remains with mayor/mpr; the deployer does not merge.

## Amendment: criterion 3 scope

Criterion 3's "0 diff-owned FAIL" held for the branch in isolation, not for
the merge with `main`. `TestSuspendAgent_PackDerived_PreservesComments`
failed against `main` after `AgentPatch.Dir` gained `omitempty`, which
stopped the encoder emitting the `dir = ""` line the test asserted. Corrected
in the maintainer fixup commit, along with the `rig`-key refusal in
`findTOMLArrayBlock` that the same `main` change made necessary.
