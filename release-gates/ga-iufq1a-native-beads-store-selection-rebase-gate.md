# Release Gate: ga-iufq1a - Native Beads Store Selection Rebase

- Gate run: 2026-05-30T07:37:03Z
- Deploy bead: ga-iufq1a
- Source review bead: ga-ivom1s
- Source build bead: ga-2lukb1
- PR: https://github.com/gastownhall/gascity/pull/2640
- Branch: builder/ga-l2souo-6-e2e
- Evaluated head: f3c9b6a543d61e66004c5eac108ec69717adebfb
- Release criteria source: deployer release-gate criteria. `docs/PROJECT_MANIFEST.md` is not present in this checkout.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-ivom1s is closed with `REVIEW VERDICT: PASS` for head f3c9b6a54 on branch builder/ga-l2souo-6-e2e. Deploy bead ga-iufq1a records reviewer PASS and no blockers. |
| 2 | Acceptance criteria met | PASS | Review evidence covers the rebased native beads store selection path: native Dolt store adapter, guarded factory selection, seven preflight gates, redacted diagnostics, `BeadDeleted` event registration, OpenAPI/schema/dashboard status projection, and force-fallback behavior. Local tests and dashboard checks below exercise those surfaces. |
| 3 | Tests pass | PASS | `GOTOOLCHAIN=auto make test-fast-parallel`, `GOTOOLCHAIN=auto go vet ./...`, `GOTOOLCHAIN=auto make check-native-dependency-surface`, `GOTOOLCHAIN=auto make dashboard-check`, `git diff --check origin/main...HEAD`, and dashboard preview smoke on 127.0.0.1:4193 all passed. |
| 4 | No high-severity review findings open | PASS | ga-ivom1s review notes list no blockers and only two non-blocking observations: non-transactional SetMetadataBatch behavior and fragile upstream error-string matching. Unresolved HIGH review finding count: 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` returned only `## builder/ga-l2souo-6-e2e...origin/builder/ga-l2souo-6-e2e`. Dashboard generation left no working-tree diff. This gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main builder/ga-l2souo-6-e2e`, `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree ee678ca27c69a35a60a913bfe789b7641ddefb40. GitHub reports PR #2640 `mergeable=MERGEABLE`. The branch is 1 behind and 15 ahead of origin/main, so merge is clean but not a fast-forward descendant. |
| 7 | Single feature theme | PASS | The commit set is one feature theme: native beads store selection and its required diagnostics, event, API/schema/dashboard projection, dependency guardrails, tests, and rebase repair. |

## Review Evidence

| Bead | Status | Verdict | Evidence |
|------|--------|---------|----------|
| ga-ivom1s | closed | PASS | Reviewer validated final rebased head f3c9b6a54, branch builder/ga-l2souo-6-e2e, PR #2640. Notes state all seven preflight gates, env race guard, secret redaction, and BeadDeleted event registration were verified. |
| ga-2lukb1 | closed | builder verification | Builder rebased PR #2640 onto origin/main twice, resolved cmd/gc/bd_env.go conflict, pushed f3c9b6a54, and reran dashboard-check, native dependency surface guard, test-fast-parallel, go vet, diff check, pre-commit, and dashboard preview smoke. |

## Test Evidence

| Command | Result |
|---------|--------|
| `git diff --check origin/main...HEAD` | PASS |
| `GOTOOLCHAIN=auto make test-fast-parallel` | PASS: all fast jobs passed. |
| `GOTOOLCHAIN=auto go vet ./...` | PASS |
| `GOTOOLCHAIN=auto make check-native-dependency-surface` | PASS: modules=725, aws=25, azure=9, dolthub=14, googleapi=1, binary_bytes=238741536. |
| `GOTOOLCHAIN=auto make dashboard-check` | PASS: OpenAPI client generation, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...` passed. |
| `npm run preview -- --host 127.0.0.1 --port 4193 --strictPort` plus `curl -fsS http://127.0.0.1:4193/` | PASS: preview served 22,831 bytes; preview process was stopped after the smoke check. |

## GitHub Check Observation

`gh pr checks 2640` reports the required summary checks as passing:
`CI / required`, `CI / preflight`, `CI / integration`, `Check`, and `Trivy`.
The rollup also includes non-required failing `CodeQL` and
`Image vulnerabilities` checks from their separate workflows. They are visible
on the PR and are not counted as release-gate review findings because the
deployer gate criteria in this checkout do not include non-required GitHub
checks as a binary criterion.

## Acceptance Evidence

- Native store selection remains gated by metadata, bd context, Dolt mode,
  identity, version compatibility, and hook-safety preflight checks.
- Fallback remains available through the existing `BdStore` path and explicit
  `GC_BEADS_FORCE_FALLBACK=1` behavior.
- Status JSON and generated schemas expose beads diagnostics without raw
  credentials in preflight details.
- `BeadDeleted` is registered and dispatched through the typed event path.
- Generated OpenAPI/schema/dashboard clients are in sync.
- Native dependency closure is guarded by
  `scripts/check-native-dependency-surface.sh`.

## Final Gate Result

PASS. The current PR head is suitable for human review and merge decision
after this gate artifact is committed and pushed.
