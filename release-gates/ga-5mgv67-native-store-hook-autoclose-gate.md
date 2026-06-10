# Release gate - native-store hook removal and in-process autoclose

Gate bead: `ga-5mgv67`  
Source build bead: `ga-frmdxd.2`  
Review bead: `ga-4lpx1z`  
PR: https://github.com/gastownhall/gascity/pull/3303  
Branch: `builder/ga-frmdxd.2`  
Head under gate: `b4ededa76015ac514cefd36f856dd418599ec57b`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, matching prior
release-gate precedent in this repo. This gate uses the active deployer release
criteria plus `TESTING.md` and the acceptance criteria recorded on
`ga-frmdxd.2`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-4lpx1z` notes record `Review Verdict: PASS` from `gascity/reviewer` for PR #3303 and cover architecture, race analysis, security, and test coverage. |
| 2 | Acceptance criteria met | PASS | `ga-frmdxd.2` required a current-main branch containing only the native-store hook/autoclose scope from commit `630cef370`. `origin/main..HEAD` is a single commit, `b4ededa76`, and the diff is limited to `.beads/config.yaml`, `cmd/gc/api_state.go`, `cmd/gc/api_state_test.go`, `cmd/gc/hooks.go`, `cmd/gc/hooks_test.go`, `cmd/gc/beads_provider_lifecycle_test.go`, `cmd/gc/lifecycle_coordination_test.go`, and `cmd/gc/molecule_autoclose_test.go`. Emergency relay, `SupervisorHTTPCheck`, and pack-release symlink paths are absent. |
| 3 | Tests pass | PASS | Local gate commands passed: `make test-fast-parallel` on rerun; `go test ./internal/beads -run TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand -count=10 -v`; `go vet ./...`; `go build ./...`; focused `go test ./cmd/gc -run 'Test.*(Hook\|Hooks\|Autoclose\|Lifecycle\|BeadProvider)' -count=1`. GitHub PR #3303 reports required CI green at the reviewed head. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-4lpx1z` lists no open HIGH findings; GitHub PR #3303 has no PR review/comment threads. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file: `## builder/ga-frmdxd.2...origin/main [ahead 1]`. After this file is committed, the branch contains the reviewed native-store change plus this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded with tree `24636aa8318ff16b4fc2ee0cab576f022abb92ec`; GitHub reports PR #3303 merge state `CLEAN`. |
| 7 | Single feature theme | PASS | The commit set is one deploy lane: stop installing/removing gc-managed bd forwarder hooks and run bead-close autoclose in-process for native-store/#3248. The emergency/doctor/pack-release lane was split to PR #3302 and is absent here. |

## Test Notes

The first `make test-fast-parallel` run failed in an unrelated
`internal/beads` timing-sensitive test:

```text
TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand: bd.slow records = 1, want 0 for fast bd command
```

The exact failed test then passed 10/10 with `go test ./internal/beads -run
TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand -count=10 -v`, and a full
`make test-fast-parallel` rerun passed all shards. The release criterion is
marked PASS based on the passing exact retry plus the passing full rerun.

## Acceptance Evidence

`ga-frmdxd.2` acceptance criteria:

| Acceptance criterion | Result | Evidence |
|----------------------|--------|----------|
| Branch is based on current `origin/main` and contains only native-store hook/autoclose scope from `630cef370`. | PASS | `origin/main..HEAD` contains one commit, `b4ededa76`, and the diff paths are confined to hook/autoclose state and tests. |
| Scope is described as dropping gc-installed bd forwarder hooks and running autoclose in-process for native-store/#3248. | PASS | PR #3303 title and review bead describe that exact scope; implementation removes hook installation and routes bead-close autoclose through `applyBeadEventToStores`. |
| Diff excludes emergency relay, `SupervisorHTTPCheck`, and macOS pack-release symlink changes. | PASS | Diff paths do not include `internal/emergency`, `internal/doctor/checks_supervisor_http.go`, doctor golden/API schema generated files, or `cmd/gc/cmd_pack_release.go`. |
| PR description links split context and explains criterion-7 deploy split. | PASS | PR #3303 already carries split context; deployer will update the body with reviewer-facing release notes and link this gate. |
| Relevant focused tests plus fast baseline, vet, and build pass. | PASS | All commands listed in release criterion 3 passed after the transient fast-baseline retry. |
| Builder routes architecture/test-plan follow-up if extraction expands scope. | PASS | Extraction stayed within the native-store hook/autoclose scope; no extra follow-up was required by the reviewer. |
| Builder handed off through normal review/deploy path and did not merge directly. | PASS | `ga-4lpx1z` is closed PASS by reviewer; `ga-5mgv67` was routed to deployer as `needs-deploy`; PR #3303 remains open. |

## Diff Summary

```text
.beads/config.yaml
cmd/gc/api_state.go
cmd/gc/api_state_test.go
cmd/gc/beads_provider_lifecycle_test.go
cmd/gc/hooks.go
cmd/gc/hooks_test.go
cmd/gc/lifecycle_coordination_test.go
cmd/gc/molecule_autoclose_test.go
```

