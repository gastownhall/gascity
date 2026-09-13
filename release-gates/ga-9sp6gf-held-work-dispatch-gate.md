# Release Gate: held work automatic-dispatch suppression

- Deploy bead: `ga-9sp6gf`
- Review bead: `ga-sijivh`
- Source bead: `ga-x9kptu`
- Reviewed commit: `ff03c7d6d2cc48693a72e4e198e9c8f276abfecc`
- Deploy branch: `deploy/ga-9sp6gf-gate`
- Source branch: `builder/ga-x9kptu` (provenance only; not a deploy push target)
- Base checked: `origin/main@85e3e5022b925c9781fb64e0b1a043133770cf72`
- Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout; this gate uses the active deployer release criteria and the repository testing policy in `TESTING.md`.

## Summary

PASS on 2026-08-03.

The change prevents unassigned, route-scoped automatic dispatch from serving
beads carrying either canonical hold label. Assignee-scoped recovery and ready
queries remain hold-transparent, preserving deliberate assignment and recovery
semantics.

## Criterion 6: branch diverges cleanly from main

PASS. Evaluated first.

- `git merge-base --is-ancestor origin/main ff03c7d6d2cc48693a72e4e198e9c8f276abfecc` returned 0.
- The merge base is `85e3e5022b925c9781fb64e0b1a043133770cf72`, the checked `origin/main` tip.
- `git merge-tree --write-tree origin/main ff03c7d6d2cc48693a72e4e198e9c8f276abfecc` returned tree `5f4ce8c7db24335bda68dae6ed410c93c68c1c53` with exit 0.
- No bounded self-rebase was needed.

## Release criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead `ga-sijivh` records `verdict: pass` at the reviewed SHA after round 2 closed both previously uncovered criteria. |
| 2 | Acceptance criteria met | PASS | Production entry-point coverage exercises control-ready cache evaluation and fallback filtering, route-scoped Tier-3 shell queries, legacy bd 1.0.4/1.0.5 query shapes, pool-demand count queries, and `buildOnBoot`/`buildOnDeath` recovery handoff. Tests preserve hold transparency for assignee-scoped Tier 1/2 paths, cover absent labels plus `hold:mayor`, `hold:external`, and both labels together, and derive enforcement from `beadmeta.DispatchHoldLabels`. RED commits `bd3449005` and `af24469ad` precede GREEN commits `82d01bb1b` and `ff03c7d6d`. Deterministic lifecycle tests use `t.TempDir()` and a local fake `bd`; the golden matrix covers supported bd semantics. |
| 3 | Tests pass | PASS | `make test-fast-parallel`: 10/10 jobs passed. Counted run `go test -json ./cmd/gc/... ./internal/beadmeta/... ./internal/config/...`: 17,169 PASS, 0 FAIL, 104 SKIP. The skips are pre-existing OS/environment/slow-process tier gates; the focused hold-label and recovery run completed 25 PASS, 0 FAIL, 0 SKIP, so no feature test was skipped. `go vet ./...`, `go build ./...`, `git diff --check origin/main...HEAD`, and `gofmt -l` over changed Go files all passed. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-sijivh` records no security, style, or specification blocker and no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | The isolated gate worktree was clean on `deploy/ga-9sp6gf-gate` at the exact reviewed SHA before this checklist was added. This gate file is the only deploy-only delta and will be committed separately. |
| 6 | Branch diverges cleanly from main | PASS | See the criterion 6 evidence above. |
| 7 | Single feature theme | PASS | All four commits and all touched packages implement one behavior: suppress held beads from ambient automatic dispatch while preserving deliberately assigned work. No independent feature is bundled. |

## Test commands

```bash
make test-fast-parallel
go test -json ./cmd/gc/... ./internal/beadmeta/... ./internal/config/...
go test -json ./cmd/gc ./internal/beadmeta ./internal/config -run 'DispatchHoldLabels|HoldLabel|BuildOnDeathReopensHeldBead|BuildOnBootReopensHeldBead|WorkflowServeControlReadyQuery.*(Hold|ShellFallback)'
go vet ./...
go build ./...
git diff --check origin/main...HEAD
gofmt -l $(git diff --name-only origin/main...HEAD -- '*.go')
```
