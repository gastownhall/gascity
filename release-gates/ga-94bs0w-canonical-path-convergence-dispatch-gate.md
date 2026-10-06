# Release gate: canonical path classification in convergence and dispatch

- Deploy bead: `ga-94bs0w`
- Build bead: `ga-iawy13.4`
- Review bead: `ga-72lu2m`
- Reviewed source: `e8b75defefb74c6844a19a722cebdbd54dbe470a`
- Deploy branch: `deploy/ga-94bs0w-gate`
- Gate base: `origin/main@0223c3af63cf5cab296f9abed25bcced5eb91794`
- Evaluation date: 2026-08-03
- Disposition: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at the reviewed commit, so this
checklist applies the deployer role's release criteria and the repository's
documented test-evidence policy.

## Gate checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first and rechecked after testing. `git merge-tree --write-tree origin/main e8b75defefb74c6844a19a722cebdbd54dbe470a` exited 0 against `origin/main@0223c3af63cf5cab296f9abed25bcced5eb91794` and produced tree `fe1ab02e397d97fade37998bea4085db92d1702d`. The source is two commits ahead and one behind current main with no content conflict; no self-rebase or source-branch mutation was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-72lu2m` is closed with reason `pass`, records `verdict: pass`, and names the exact reviewed source SHA. The reviewer independently verified the classification, tests, formatting, and path-containment security behavior. |
| 2 | Acceptance criteria met | **PASS** | All nine scoped `filepath.EvalSymlinks` sites are classified in the matrix below. The three comparison-preparation inputs use `pathutil.NormalizePathForCompare` at subsystem entry; the six existence/resolvability checks remain bare with adjacent `canonical-path-exception` justification. New tests cover relative and symlinked spellings, missing paths, contained targets, and symlink escapes. The focused package suite and vet pass, and no scoped production call remains unexplained. |
| 3 | Tests pass | **PASS** | At the exact reviewed SHA, documented `make test-fast-parallel` completed **10 PASS jobs, 0 FAIL jobs, 0 SKIP jobs**. `go build ./...` and `go vet ./...` exited 0. A fresh JSON run of `go test -count=1 ./internal/convergence/... ./internal/dispatch/...` recorded **738 PASS, 0 FAIL, 0 SKIP**. `git diff --check origin/main...HEAD` also passed. |
| 4 | No high-severity review findings open | **PASS** | Reviewer notes report no specification, style, security, compatibility, or uncovered-criteria blockers. Unresolved HIGH/CRITICAL findings: 0. |
| 5 | Final branch is clean | **PASS** | Before adding this checklist, `git status --porcelain=v1 --untracked-files=all` produced no output. The configured hook path is `.githooks`; this checklist is the sole deployer-authored release change and will be committed before push. |
| 7 | Single feature theme | **PASS** | The two-commit TDD set changes one canonical-path-at-ingest behavior across the coupled convergence and dispatch path-validation surfaces. All eight changed files are implementation or adjacent tests for that theme; no independent feature is bundled. |

## Per-site classification

| Site | Behavior class | Disposition |
|---|---|---|
| `internal/convergence/artifact.go` — artifact root | Existence/resolvability | Keep `EvalSymlinks`; a missing or unresolvable artifact directory must fail. |
| `internal/convergence/artifact.go` — walked symlink target | Existence/resolvability | Keep `EvalSymlinks`; a dangling or unresolvable target must fail validation. |
| `internal/convergence/condition.go` — envelope | Comparison preparation | Normalize once with `pathutil.NormalizePathForCompare`. |
| `internal/convergence/condition.go` — base | Comparison preparation | Normalize once with `pathutil.NormalizePathForCompare`. |
| `internal/convergence/condition.go` — condition script | Existence/resolvability | Keep `EvalSymlinks`; the script must resolve to an executable file. |
| `internal/convergence/evaluate.go` — city path | Comparison preparation | Normalize once with `pathutil.NormalizePathForCompare`. |
| `internal/convergence/evaluate.go` — prompt path | Existence/resolvability | Keep `EvalSymlinks`; preserve the explicit symlink-presence check and deferred missing-file behavior. |
| `internal/dispatch/retry.go` — worktree root | Existence/resolvability | Keep `EvalSymlinks`; fail closed if the worktree root does not resolve. |
| `internal/dispatch/retry.go` — required artifact target | Existence/resolvability | Keep `EvalSymlinks`; preserve missing-target tolerance while rejecting a resolved target outside the worktree. |

## Acceptance evidence

- `TestResolveConditionPath/relative_envelope_combined_with_a_symlinked_conditionPath_segment_must_not_be_falsely_rejected` proves relative and symlinked spellings converge on the same containment decision.
- `TestResolveEvaluateStep_RelativeCityPathReturnsAbsolutePromptPath` proves a relative city path produces a canonical absolute prompt path.
- `TestValidateArtifactDir_MissingDir` preserves the artifact-root existence failure.
- `TestRequiredArtifactTargetInWorktree` covers a missing target, a symlinked worktree root with a contained target, and a symlink escape outside the worktree.
- No API, configuration, persistence, generated-schema, or dependency change is included.
