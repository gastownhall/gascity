# Release Gate: Canonical path ingest for formulas, workflows, and skills

- Deploy bead: `ga-sdcjgv`
- Build bead: `ga-iawy13.6`
- Review bead: `ga-q8rpff`
- Reviewed commit: `775129cb25b1b96e077eeb85c442e912d58c0dce`
- Final rebased code commit: `81c5073cc6a8c19c30e70af83928b5fa5fa052b8`
- Isolated branch: `deploy/ga-sdcjgv-gate`
- Base: `origin/main` at `c4880aef5f2c6be534358f09354c1d249e32161c`
- Overall result: **PASS**

The repository does not contain `docs/PROJECT_MANIFEST.md` at this revision.
This checklist therefore applies the canonical seven deployer release criteria
plus the repository requirements in `AGENTS.md`, `TESTING.md`, and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Criterion 6 evaluated first

**PASS.** The final code commit is cleanly based on `origin/main`.

- `git merge-base --is-ancestor origin/main 81c5073c` returned `0`.
- `git merge-tree --write-tree origin/main 81c5073c` returned tree
  `d62e74e354413ca917c27bc93dd11483a9b1d43e` with exit `0`.
- `origin/main` resolved to
  `c4880aef5f2c6be534358f09354c1d249e32161c`.
- The code-only remote deploy ref resolved to
  `81c5073cc6a8c19c30e70af83928b5fa5fa052b8` before evaluation.

No additional self-rebase was required during this gate cycle.

## Acceptance evidence

All five scoped production sites are comparison or identity preparation and
delegate to `pathutil.NormalizePathForCompare` on the final code commit:

| Site | Classification | Disposition |
| --- | --- | --- |
| `internal/formula/parser.go:descriptionFileBaseDir` | Description-file anchor preparation | Normalize once before deriving the directory. |
| `internal/formula/source.go:canonicalExistingPath` | Cache-key and `filepath.Rel` preparation | Delegate to the shared normalizer, including multi-level missing tails. |
| `internal/sourceworkflow/sourceworkflow.go:canonicalScopeRef` | Workflow lock identity | Preserve the empty sentinel; otherwise normalize to a canonical absolute path. |
| `internal/sourceworkflow/sourceworkflow.go:canonicalCityPath` | Workflow lock identity plus empty-path validation | Preserve validation and normalize the accepted path once. |
| `internal/materialize/skills.go:canonicalizePath` | Ownership-root and containment comparison | Preserve the call-site contract and delegate to the shared normalizer. |

`rg 'filepath\.EvalSymlinks|EvalSymlinks'` over the four scoped production
files returned no matches. The final two-commit diff is confined to seven
files in the formula, source-workflow, and skill-materialization canonical-path
theme. Regression tests cover symlinked parents, missing leaves, multi-level
missing tails, and absolute lock identities; existing materializer containment
coverage remains in place.

## Test evidence integrity

The changed `internal/**` Go paths activate the required process-backed
`cmd/gc` and PR integration lanes in `.github/workflows/ci.yml`. The final
evidence ran those documented sharded lanes with the CI-pinned `bd v1.1.0`
and Dolt `2.1.7`, a short on-disk `/var/tmp` fixture root, and tmux `3.4` for
the tmux matrix.

- `make test-fast-parallel`: **10 PASS, 0 FAIL, 0 SKIP** at job level.
- Process-backed `cmd/gc`: **6 PASS, 0 FAIL, 0 SKIP** local shards, plus
  product-metrics testhook **1 PASS, 0 FAIL, 0 SKIP**.
- PR integration coverage: core packages **4 PASS**, integration-tagged
  `cmd/gc` **6 PASS**, runtime tmux **6 PASS**, bdstore **1 PASS**, REST smoke
  **2 PASS**; total **19 PASS, 0 FAIL, 0 SKIP** at shard/job level.
- Additional formula-review integration jobs completed **5 PASS, 0 FAIL,
  0 SKIP**.
- `go test -count=1 -json ./internal/formula ./internal/sourceworkflow
  ./internal/materialize`: **796 PASS, 0 FAIL, 1 SKIP**. The skip is
  `TestCompileBugReportFlowV2`, whose unrelated external fixture
  `/home/ubuntu/tooling/formulas/mol-bug-report-flow-v2.toml` is absent.
- `go vet ./...`: exit `0`.
- `go build ./...`: exit `0`.

Two setup diagnostics are deliberately excluded from the counts above: an
initial descriptive temp path exceeded the Unix socket length limit, and a
clean HOME override was rejected by the platform-supervisor contract. Both
runs were interrupted after diagnosis. Every affected required shard was then
rerun in a valid job-specific environment and passed; no PASS is inferred from
either interrupted diagnostic.

## Release criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | **PASS** | `ga-q8rpff` is closed with reason `pass`; its notes record `verdict: pass`, no uncovered criteria, and no blocker/major/security findings. |
| 2 | Acceptance criteria met | **PASS** | The per-site classification matrix covers every scoped production site. All comparison sites use the shared canonicalizer, no scoped bare call remains, validation and call-site contracts are preserved, and the required symlink/missing-tail regressions are covered. |
| 3 | Tests pass | **PASS** | All path-required CI-equivalent lanes passed with the counts and environment evidence above. The one targeted skip is external-fixture-only and does not exercise this change. |
| 4 | No high-severity review findings open | **PASS** | Review notes report no blocker, major, HIGH, or CRITICAL findings. Unresolved high-severity finding count: **0**. |
| 5 | Final branch is clean | **PASS** | The detached evaluation worktree remained pinned to `81c5073c` before and after testing with zero status entries. The isolated deploy branch was reset mechanically to that SHA and was clean before this checklist was added. |
| 6 | Branch diverges cleanly from main | **PASS** | Evaluated first; see the dedicated section above. |
| 7 | Single feature theme | **PASS** | The two feature commits implement one coherent canonical-path-at-ingest change across formula, workflow-lock, and skill-materialization comparison boundaries. No independent feature is bundled. |

## Gate disposition

The gate passes. Commit this checklist on the isolated deploy branch, push that
branch only after the shared-branch safety guard passes, open the PR, and route
the verified merge-request to the merge authority. The deployer does not merge.
