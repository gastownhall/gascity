# Release gate: pool identity in push ownership guard

- Deploy bead: `ga-yaqkq6`
- Reviewed source: `356b611b21462839364d72f9f5c03e27ff4f02c9`
- Gated head after bounded self-rebase: `f176cbff84a1ab31317c7af9b2bf26429bf122d8`
- Base: `origin/main@e736d74d0a84a129de47f9008c4560d3146c77ce`
- Deploy mode: `remote`; push remote: `origin`
- Overall verdict: **PASS**

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-qdljcu` records `REVIEW VERDICT: PASS` for the reviewed source and independently verifies build, vet, shell syntax, shellcheck, the regression suite, and the trust-boundary change. |
| 2 | Acceptance criteria met | **PASS** | `scripts/push-ownership-guard.sh` now accepts `GC_TEMPLATE` as a valid assignee identity. The regression `allow/assignee-is-bare-pool-template` passes when the pool identity is `tmpl-x` and all instance-shaped identity values differ. The fail-closed reassignment case also passes. |
| 3 | Tests pass | **PASS with attributed raw failures** | The documented union, `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock TESTCONTAINERS_RYUK_DISABLED=true make test-local-full-parallel`, completed **33 PASS / 7 FAIL / 0 SKIP jobs**. All seven red jobs are retained and attributed below under criterion 3a. The first invocation executed no tests and returned infrastructure admission code 75 after the shared two-slot gate stayed full for 600 seconds; the permitted pre-test retry acquired a slot and produced the recorded union result. `go build ./...`, `go vet ./...`, `shellcheck` on both changed scripts, `bash -n` on both changed scripts, and `git diff --check` all pass. |
| 3b | Policy/lint lane | **PASS** | `make test-ci-policy` passes: 5 runner-policy tests, 15 CI-coverage tests, `go test ./scripts/cipolicy`, and the focused static-scope contracts are green. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-qdljcu` records PASS with zero findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | `git status --porcelain` was empty after all gate commands and before this checklist was created. |
| 6 | Branch diverges cleanly from main | **PASS** | The reviewed source was stale but `git merge-tree --write-tree` was clean. The canonical bounded helper rebased and force-with-lease pushed `356b611b21462839364d72f9f5c03e27ff4f02c9 -> f176cbff84a1ab31317c7af9b2bf26429bf122d8` with rc 0. Local and remote heads match, and current `origin/main` is an ancestor of the gated head. |
| 7 | Single feature theme | **PASS** | The final diff is limited to `scripts/push-ownership-guard.sh` and its dedicated shell regression suite: one push-ownership identity-normalization theme. |

## Criterion 3 evidence

Test environment:

- Rootless Podman socket is live at `unix:///run/user/1000/podman/podman.sock`.
- Cached images include repository-pinned `dolthub/dolt:2.1.7` and testcontainers image `dolthub/dolt-sql-server:1.32.4`.
- `TESTCONTAINERS_RYUK_DISABLED=true` was set before the union.

Diff-owned tests:

- `scripts/test-push-ownership-guard.sh`: **36 PASS / 0 FAIL / 0 SKIP**.
- `allow/assignee-is-bare-pool-template`: **PASS** by name.
- The existing Go `scripts` package wrapper also passed in the union.
- `diff_tests_executed`: all 36 named cases in the modified shell suite PASS; no diff-owned test skipped.
- `waiver_ref`: `MAYOR-2026-08-19-ga-yaqkq6-full-disposition` is recorded on the bead, but the current result is independently adjudicated under the live three-outcome criterion-3 policy.

Raw failures and attribution:

- `TestBdFlagManifestCurrent` in `integration-packages-core-1-of-4` -> `ga-gqxh5s`. Clause 3(a), mechanism: the installed `bd --help` exposes flags absent from `internal/bdflags`; the candidate changes only push-guard shell files and cannot alter either the installed binary or the Go manifest. Tracker created 2026-07-28, before this run.
- `TestProviderLiveClaudeKindPath` in `unit-core` and `integration-packages-core-3-of-4` -> `ga-fh1flg`. Clause 3(a), mechanism: both occurrences have the tracked `agent_pane_busy` / startup-delivery timeout signature. No failing herdr package path imports or invokes the changed push-guard scripts. Tracker created 2026-08-18, before this run; standing disposition `mayor-2026-08-20-herdr-pane-standing` also covers the exact signature.
- `TestGetKeyBinding_CapturesDefaultBinding` and `TestGetKeyBinding_CapturesDefaultBindingWithArgs` -> `ga-afqddr`. Clause 3(a), mechanism: host tmux 3.7b returns an empty filtered default key table (`next-window` / `choose-tree` missing). The candidate cannot reach `internal/runtime/tmux`. Tracker created 2026-08-15 and explicitly names both tests.
- `TestAdoptPRFormulaCompileAndRun` and `TestPersonalWorkFormulaCompileAndRun` -> `ga-lpfjhc`. Clause 3(a), mechanism: both fail during fixture `gc init`, before formula execution, with the exact gastownhall/beads#4566 `pending schema migrations alter pre-existing dirty tables` signature. Push-guard shell code cannot alter Dolt schema migration or store bootstrap. Tracker created 2026-08-15; standing authorization is recorded on `ga-6bnc42`.

For every attributed failure: the failing test file is not diff-owned; the cited tracker predates the run and covers the exact test or condition; `git grep` found no push-guard reference in `internal/bdflags/**`, `internal/runtime/herdr/**`, `internal/runtime/tmux/**`, or `test/integration/**`; and none of those paths overlaps the two changed files. The diff changes no resource census, Makefile, workflow, or test target.
