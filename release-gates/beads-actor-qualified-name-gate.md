# Release gate: align BEADS_ACTOR with the stable agent alias

- Deploy bead: `ga-d6zqfj`
- Build bead: `ga-jav9u9`
- Source review: `ga-mfccbk`
- Reviewed feature commit: `941812ce44b193ebfc3ab3903861bcf79467e3e3`
- Gate source tip: `072f6da09f487274385eedb1e3d8a83b228ed8b8`
- Main evaluated: `origin/main@4f127d926ea346f8fa97055af87c6afaf5ea13bb`
- Deploy branch: `deploy/ga-d6zqfj-gate`
- Evaluated: `2026-08-04`
- Overall verdict: **PASS**

`docs/PROJECT_MANIFEST.md` is not present at the evaluated commit. This
checklist therefore applies the deployer role's seven release criteria and
`engdocs/contributors/release-gate-criteria-conventions.md`.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first after fetching `origin/main` and `origin/builder/ga-jav9u9`. `origin/main@4f127d926ea346f8fa97055af87c6afaf5ea13bb` is an ancestor of the remediated source tip `072f6da09f487274385eedb1e3d8a83b228ed8b8`; no self-rebase was needed. |
| 1 | Review PASS present | **PASS** | Review bead `ga-mfccbk` is closed with reason `pass`; its notes record `verdict: pass` for reviewed feature commit `941812ce44b193ebfc3ab3903861bcf79467e3e3`. |
| 2 | Acceptance criteria met | **PASS** | `cmd/gc/template_resolve.go` now sets `BEADS_ACTOR` to `qualifiedName`, matching `GC_ALIAS`, rather than the session name. `TestResolveTemplateSetsBeadsActorToQualifiedNameNotSessionName` proves the session name differs and asserts both identity values match; focused verification ran 2 PASS, 0 FAIL, 0 SKIP. |
| 3 | Tests pass | **PASS** | The changed `cmd/gc/**` paths require the `cmd_gc_process` CI lane. `make test-cmd-gc-process-parallel` ran 7 jobs: 7 PASS, 0 FAIL, 0 SKIP (six process shards plus `productmetrics-testhook`). Required fast baseline `make test-fast-parallel` ran 10 jobs: 10 PASS, 0 FAIL, 0 SKIP. `go vet ./...` also passed. |
| 4 | No high-severity review findings open | **PASS** | Review bead `ga-mfccbk` records no style or security findings and no blocker, major, minor, or HIGH finding; unresolved HIGH count is 0. |
| 5 | Final branch is clean | **PASS** | The isolated deploy worktree was clean at the evaluated source before this checklist update; after committing the checklist, `git status --short` was rechecked and was empty. |
| 7 | Single feature theme | **PASS** | The commit set is one `cmd/gc` identity-environment fix: align `BEADS_ACTOR` with the stable qualified alias and cover it with regression tests. The additional test-only HOME isolation changes remediate the prior environmental gate failure and introduce no independent product behavior. |

## Test evidence

- CI-equivalent: `make test-cmd-gc-process-parallel` — 7 PASS, 0 FAIL, 0 SKIP jobs.
- Fast baseline: `make test-fast-parallel` — 10 PASS, 0 FAIL, 0 SKIP jobs.
- Focused acceptance: `go test ./cmd/gc -run '^(TestResolveTemplateSetsBeadsActorToQualifiedNameNotSessionName|TestBdRuntimeEnvPreservesInheritedBeadsActor)$' -count=1 -v` — 2 PASS, 0 FAIL, 0 SKIP tests.
- Static analysis: `go vet ./...` — PASS.
