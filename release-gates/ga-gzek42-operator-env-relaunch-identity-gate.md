**Verdict:** **PASS**

# Release gate: operator-authored env relaunch identity (ga-gzek42)

- Reviewed source: `623fc2656cc3e61e663c07bf2f5166633cc0ea3c`
- Base: `origin/main@9877ab55656f8c2698a82dab537e9eaad99a5866`
- Deploy mode: `remote`; push remote: `fork`
- Existing-PR preflight: no pull request carries the reviewed SHA; the SHA is not on `origin/main`
- Criteria source: the active deployer release criteria and `TESTING.md`. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Review bead `ga-a315ml` is closed with `verdict: pass`, `tests_green: true`, no uncovered criteria, and deploy commit `623fc2656cc3e61e663c07bf2f5166633cc0ea3c`. |
| 2 | Acceptance criteria met | PASS | The two-file diff captures exactly the expanded workspace, resolved-provider, and agent env layers in last-wins order; clones the resulting map into `runtime.Config.OperatorEnv`; excludes passthrough/generated/upstream-credential values; scrubs the controller token; and relies on the existing Launch-tier fingerprint/relaunch path. The four diff-owned tests below pass. `bd dep list` confirms no dependency edge to rollout co-requisites `ga-i3yh2a` or `ga-580naz`. No `config.Agent` field was added. |
| 3 | Tests pass | PASS (one attributed pre-existing failure) | Full-scope command and attribution are recorded below. The only raw failure is the tracked concurrent-initializer schema-ceiling condition and is not caused by this diff. All diff-owned tests pass. |
| 4 | No high-severity review findings open | PASS | Reviewer recorded no style or security blockers, no uncovered criteria, and no unresolved HIGH finding. |
| 5 | Final branch clean | PASS | Candidate was checked out at the exact reviewed SHA; `git status --porcelain=v1` was empty and `git diff --check origin/main...HEAD` passed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main 623fc2656cc3e61e663c07bf2f5166633cc0ea3c` exited 0 and produced tree `db8ede179cd864a86cf5768d453d3b8d36c79d10`. Merge base: `c814d5a5c349617e36ec85c345b99a4aaa9fbe75`. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The range changes only `cmd/gc/template_resolve.go` and `cmd/gc/template_resolve_env_test.go`, all within the operator-authored config-env identity/relaunch feature. |

## Criterion 2 acceptance evidence

1. `operatorEnv := mergeEnv(expandEnvMap(workspaceEnv), expandEnvMap(resolved.Env), expandEnvMap(cfgAgent.Env))` preserves the runtime env layer order.
2. `convergence.ScrubTokenEnv(operatorEnv)` and `maps.Clone(tp.OperatorEnv)` provide token scrubbing and map detachment; passthrough/generated/upstream credential layers are excluded.
3. Workspace-default and explicit resolved-provider selection are covered by the diff-owned template-resolution tests; the explicit-provider path is the post-compose shape used by rig patches.
4. Existing fingerprint partition tests classify `OperatorEnv` as Launch-tier, and the reconciler compares the whole Launch fingerprint before using the existing session-key-preserving relaunch path.
5. The upstream credential is proven present in runtime `Env` but absent from `OperatorEnv`, so credential rotation does not move the new identity.
6. No `config.Agent` field was introduced.
7. Full gate suite, policy lane, focused diff-owned confirmation, and `go vet ./...` were run at the reviewed SHA.
8. `ga-i3yh2a` and `ga-580naz` remain recommendations only; neither is a dependency of `ga-evj082` or its review bead.

## Criterion 3 test evidence

- Environment: rootless Podman 5.8.4 available at `unix:///run/user/1000/podman/podman.sock`; `TESTCONTAINERS_RYUK_DISABLED=true`. No `dolt-tests-via-podman` cairn entry or Gas City testcontainer image pin exists.
- test_cmd: `isolated-test-run.sh -- bash -c 'make test-local-full-parallel'`
- test_cmd_scope: `full-suite`
- test_counts: 40 supervised jobs; 39 PASS, 1 raw FAIL, 0 job SKIP; one top-level test failure and zero reported top-level test skips; no isolation tripwire.
- raw_failure: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix` in `cmd-gc-process-6-of-6` stopped during fixture `bd init`: database `hq` appeared mid-migration and external bd refused five pending shared-server migrations (`v61 -> v66`) before feature assertions. Log: `/var/tmp/ga-gzek42-full-suite.2fPl53/cmd-gc-process-6-of-6.log`.
- failure_attribution: `TestFreshManagedBdCityInitSeedsPinnedHQDatabaseAndKeepsGCPrefix -> ga-lejnse | clause 3(b) CROSS-PR`. The open tracker predates this run and records the exact test/signature on unrelated `internal/api`, `internal/molecule`, shell-only, and other candidates. Same-package clause 4 is satisfied by that external proof. Added-test-load guard: no new test file, resource-census baseline, Makefile target, workflow, or other new suite target. Sighting comment `1cca90c8-c509-5efa-b9da-53d5f913812e` was read back from the tracker.
- diff_tests_executed:
  - `TestResolveTemplatePopulatesOperatorEnvFromOperatorAuthoredLayers`: PASS (full-suite shard 1/6; focused verbose confirmation PASS)
  - `TestResolveTemplatePopulatesOperatorEnvWithExplicitProviderSelection`: PASS (full-suite shard 2/6; focused verbose confirmation PASS)
  - `TestResolveTemplateExcludesUpstreamCredentialFromOperatorEnv`: PASS (full-suite shard 3/6; focused verbose confirmation PASS)
  - `TestResolveTemplateScrubsControllerTokenFromOperatorEnv`: PASS (full-suite shard 4/6; focused verbose confirmation PASS)
- supplementary diff-owned command: `go test -count=1 -v ./cmd/gc -run '^(TestResolveTemplatePopulatesOperatorEnvFromOperatorAuthoredLayers|TestResolveTemplatePopulatesOperatorEnvWithExplicitProviderSelection|TestResolveTemplateExcludesUpstreamCredentialFromOperatorEnv|TestResolveTemplateScrubsControllerTokenFromOperatorEnv)$'` through `isolated-test-run.sh`: 4 PASS, 0 FAIL, 0 SKIP.
- policy_lane: `make test-ci-policy` through `isolated-test-run.sh`: PASS (two Python policy suites, `scripts/cipolicy`, `scripts/prwatchdog/...`, and static-scope contract tests all green).
- vet: `go vet ./...`: PASS.
- ci_lane_run: n/a; the diff changes no CI workflow, matrix, timeout, or required-check list.
- skip_justification: none; no test skip was reported.
- waiver_ref: none; this gate uses ordinary pre-existing-failure attribution, not a waiver.

