# Release Gate: Resource test-target census (`ga-oohbih`)

- **Verdict:** PASS
- **Reviewed source:** `591b819a8fc58cfd881fa6cdb0c11e3b3c85e251`
- **Build bead:** `ga-4ag4p2.1`
- **Review bead:** `ga-x940sh`
- **Base evaluated:** `origin/main@c3b12fb91ac53cfddcbac9ac3aba43d5c3ddcf8f`
- **Evaluated:** 2026-08-25
- **Deploy mode:** remote; push remote `origin`
- **Already-merged pre-flight:** no PR carries the reviewed commit; normal gate path applies

## Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | Closed review bead `ga-x940sh` records `REVIEW VERDICT (gascity/reviewer): PASS` for the resolved 40-character source commit. |
| 2 | Acceptance criteria met | PASS | The complete `internal/testpolicy/resourcecensus` package passed. The four new fixture tests prove Makefile and workflow `run:` matches, multiple-file/multiple-line counts, and rejection of commented/non-`run:` text. `TestRepositoryLedgerMatchesCensusAndDocumentation` passed against the live reviewed tree, validating the 42-call/4-file baseline, ownership fields, TOML mirror, and generated `TESTING.md` row. |
| 3 | Tests pass | PASS (attributed failures) | The documented full union ran at the reviewed SHA: `LOCAL_TEST_JOBS=4 make test-local-full-parallel` with the rootless Podman socket configured. Raw result: **35 PASS / 5 FAIL / 0 SKIP jobs**. All five red jobs are preserved below and satisfy the pre-existing-failure attribution rule; the diff-owned package independently reported **210 PASS / 0 FAIL / 0 SKIP** result lines. |
| 3b | Policy/lint lane | PASS | `make test-ci-policy` PASS; PR-scoped `LINT_CHANGED_SCOPE=tracked LINT_CHANGED_REF=1807cf018045e9f225993d97cf6daea37e2ce6e9 LINT_FLAGS=--allow-parallel-runners make lint-affected` PASS with `0 issues`; `make fmt-check-changed` PASS; `make vet` PASS. |
| 4 | No high-severity review findings open | PASS | Reviewer requested no changes and recorded no HIGH findings; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short` was empty at the exact reviewed source before the gate artifact; the gate file is committed as the only deploy-only change and cleanliness is rechecked after commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main 591b819a8fc58cfd881fa6cdb0c11e3b3c85e251` exited 0 and produced tree `61118ec0f9ddd6b341bcfa3b6b584c088e4f669b`. Merge base is `1807cf018045e9f225993d97cf6daea37e2ce6e9`; source is 2 commits ahead and 5 behind current main. No self-rebase was needed. |
| 7 | Single feature theme | PASS | The two TDD commits touch only the resource-census implementation/test plus its checked TOML and `TESTING.md` mirrors. `assert_deploy_ancestry_scope origin/main <source> ga-oohbih ga-4ag4p2.1` passed; no unrelated or `.claude/**` ancestry is present. |

## Acceptance evidence

1. `go test -count=1 -v ./internal/testpolicy/resourcecensus` passed with 210 PASS, 0 FAIL, 0 SKIP result lines. `TestScanBuildTargetGoTestLineIncrementsCount` explicitly proves `ScopeBuildTargets`/`ResourceTestTarget` becomes non-zero.
2. `TestScanBuildTargetWorkflowRunLineIncrementsCount`, `TestScanBuildTargetCountsMultipleFilesAndLines`, and `TestScanBuildTargetSkipsCommentedAndNonRunGoTestText` all reported PASS. Together they cover Makefile recipes, workflow `run:` fields, multiple lines/files, comments, and non-`run:` YAML text.
3. The code-owned Debt row is `build_targets/test_target = 42 calls / 4 files`, owned by `ga-4ag4p2.1`, expiring 2026-10-01. `TestRepositoryLedgerMatchesCensusAndDocumentation` reported PASS, exercising exact policy ownership, TOML mirroring, live count, and markdown rendering.
4. The full required union executed the unit-core, six non-fast `cmd/gc` process shards, product-metrics hook, integration packages, formula, bdstore, and REST shards. The resource-census package passed inside `unit-core`; all raw failures are attributed below.
5. The checked ledger row appears in `TESTING.md` through the existing renderer; no rendering implementation changed, and the repository ledger/render round-trip test passed.

## Test evidence integrity

- `test_cmd: LOCAL_TEST_JOBS=4 make test-local-full-parallel`
- `test_cmd_scope: full-suite`
- `test_counts: 35 PASS / 5 FAIL / 0 SKIP jobs`
- `skip_justification: none (zero skips)`
- `waiver_ref: ga-6bnc42` for the one beads#4566 dirty-schema failure only
- `diff_tests_executed:`
  - `TestScanBuildTargetGoTestLineIncrementsCount` — PASS
  - `TestScanBuildTargetWorkflowRunLineIncrementsCount` — PASS
  - `TestScanBuildTargetCountsMultipleFilesAndLines` — PASS
  - `TestScanBuildTargetSkipsCommentedAndNonRunGoTestText` — PASS

The first full-suite invocation did not run tests: it exited with the harness's documented temporary lock result after waiting 600 seconds for a slot. It is excluded from the result counts. The recorded retry acquired a slot and ran all 40 jobs.

### Pre-existing failure attribution

- `failure_attribution: TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/{named_session_post-kill,pool_respawn_after_drain} -> ga-hgjlhi | clause 3: b (cross-PR) — exact "async starts did not finish" signature is tracked across unrelated diffs and tied to the same hard-coded five-second async-start wait. Clause 1: not diff-owned. Clause 2: tracker created 2026-07-25 and opened during this gate. Clause 4: failing path cmd/gc/session_reconciler_trace_integration_test.go has no overlap with this diff.`
- `failure_attribution: TestBdFlagManifestCurrent (17 subtests) -> ga-gqxh5s | clause 3: d (base-ref reproduction) — tracker records the identical installed-bd flag skew on a zero-diff origin/main tree and across unrelated branches. Clause 1: not diff-owned. Clause 2: tracker created 2026-07-28 and opened during this gate. Clause 4: internal/bdflags has no path overlap with this diff.`
- `failure_attribution: TestGetKeyBinding_CapturesDefaultBinding and TestGetKeyBinding_CapturesDefaultBindingWithArgs -> ga-sxinl6 | clause 3: d (base-ref reproduction) — tracker records both exact host-tmux binding failures on a clean origin/main tree. Clause 1: not diff-owned. Clause 2: tracker created 2026-07-28 and opened during this gate. Clause 4: internal/runtime/tmux has no path overlap with this diff.`
- `failure_attribution: TestAdoptPRFormulaSoftFailsGeminiAfterTransientRetries -> ga-lpfjhc | clause 3: b (cross-PR) — exact gastownhall/beads#4566 "pending schema migrations alter pre-existing dirty tables: issues" signature is tracked across many unrelated diffs. Clause 1: not diff-owned. Clause 2: tracker created 2026-08-15 and opened during this gate. Clause 4: test/integration has no path overlap with this diff. Raw result: FAIL — WAIVED under mayor standing authorization ga-6bnc42; occurrence was appended and verified on ga-lpfjhc.`

All clause-3 proofs are decisive, so the inconclusive-path reachability/test-load guard is not invoked.

## Static-gate diagnostic

An exploratory full-repository `make lint` was not used as gate evidence: the shared default golangci cache emitted stale findings for already-deleted `/var/tmp` worktrees plus unrelated generated/vendor paths. The required changed-PR lane was then rerun with a fresh on-disk lint cache and the supported parallel-runner flag; it completed with `0 issues`. Full-repository formatting and standalone `go vet ./...` were also clean.

## Amendment — 2026-08-25, post-gate baseline refresh

The figures recorded above describe the original evaluation at reviewed source
`591b819a8fc58cfd881fa6cdb0c11e3b3c85e251` against base
`origin/main@c3b12fb91ac53cfddcbac9ac3aba43d5c3ddcf8f`. They are retained
unaltered as the historical record of that run.

After that evaluation, `2cd07e018b` ("Fail closed when required PR CI evidence
is missing", #5576) landed on `main` and added a 43rd declared `go test`
invocation line to the repository `Makefile`:

```make
	$(TEST_ENV) GOFLAGS= GOENV=off GOWORK=off go test -count=1 ./scripts/prwatchdog/...
```

Because `build_targets/test_target` is an exact-equality ratchet, the
`pull_request` merge commit counted 43 against the checked baseline of 42 and
`TestRepositoryLedgerMatchesCensusAndDocumentation` failed in CI. The new
`.github/workflows/pr-evidence-watchdog.yml` from the same commit contributes
zero occurrences and is not implicated; the file count stays at 4.

Applied on this branch:

- Merged current `origin/main` (no rebase, so the reviewed SHA above remains in
  ancestry and this record stays resolvable).
- Bumped the `build_targets`/`test_target` row from 42 to 43 calls in both
  sources of truth that `comparePolicyFields` requires to agree — the
  `bootstrapPolicy` row in `internal/testpolicy/resourcecensus/census.go` and
  the mirrored row in `test/test-resources.toml`. `reported_*` was bumped
  alongside `baseline_*` so the rendered row keeps no historical-census suffix,
  which is correct for a dimension that has no pre-AST census.
- Regenerated the `TESTING.md` ledger block via the supported
  `-run TestRepositoryLedgerMatchesCensusAndDocumentation -update` path.

The reviewed source is now a merge commit rather than
`591b819a8fc58cfd881fa6cdb0c11e3b3c85e251`. A deployer re-gate is warranted
before merge if the gate policy requires the verdict to name the exact tip.
