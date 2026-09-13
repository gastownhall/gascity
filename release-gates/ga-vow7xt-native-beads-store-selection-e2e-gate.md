# Release Gate: ga-vow7xt - Native Beads Store Selection E2E

Date: 2026-05-30

Bead: ga-vow7xt - needs-deploy: PR #2640 native beads store selection e2e
PR: https://github.com/gastownhall/gascity/pull/2640
Branch: origin/builder/ga-l2souo-6-e2e
Reviewed commit: 93c4e993b74b33aba2dc0f403efcc8b3e8ff99b7

Note: docs/PROJECT_MANIFEST.md is not present in this checkout. This gate uses
the deployer prompt's release criteria table as the release criteria source.

## Gate Summary

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review beads ga-bpaeft and ga-mopwgp are closed with `Review verdict: PASS`. Deploy bead ga-vow7xt records reviewer PASS at commit 93c4e993b74b33aba2dc0f403efcc8b3e8ff99b7. |
| 2 | Acceptance criteria met | PASS | PR behavior and review evidence cover native Dolt store selection when preflight passes, BdStore fallback with diagnostics when preflight fails, `GC_BEADS_FORCE_FALLBACK=1`, redacted preflight details, status JSON/schema/dashboard updates, dependency surface guardrails, and benchmark evidence. |
| 3 | Tests pass | PASS | `GOTOOLCHAIN=auto make test-fast-parallel` passed all fast shards; `GOTOOLCHAIN=auto go vet ./...` passed; `GOTOOLCHAIN=auto make dashboard-check` passed; dashboard preview served on 127.0.0.1:4187 and returned HTML via curl. |
| 4 | No high-severity review findings open | PASS | ga-bpaeft notes: no blockers. ga-mopwgp notes: no blockers. ga-vow7xt description: no outstanding blockers. Unresolved HIGH finding count: 0. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` returned only `## deploy/ga-vow7xt...origin/builder/ga-l2souo-6-e2e`; this gate file is the only deployer-added change and will be committed before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exited 0 and produced tree 8aa6cf5d491592e9e8035377091c2ad86a1d0602. GitHub reports PR #2640 `mergeable=MERGEABLE`, `mergeStateStatus=BLOCKED` pending normal review/check state. |
| 7 | Single feature theme | PASS | The commit set is one feature theme: native beads store selection and its required diagnostics, API/schema/dashboard projection, dependency guardrails, tests, and benchmark/recovery evidence. |

## Review Evidence

| Bead | Status | Verdict | Evidence |
|------|--------|---------|----------|
| ga-bpaeft | closed | PASS | Dependency bump reviewed at commit 93c4e993b74b33aba2dc0f403efcc8b3e8ff99b7; no blockers. |
| ga-mopwgp | closed | PASS | Benchmark evidence reviewed for PR #2640; methodology and results accepted; no blockers. |

## Test Evidence

- PASS: `GOTOOLCHAIN=auto make test-fast-parallel`
- PASS: `GOTOOLCHAIN=auto go vet ./...`
- PASS: `GOTOOLCHAIN=auto make dashboard-check`
- PASS: `npm run preview -- --host 127.0.0.1 --port 4187` from `cmd/gc/dashboard/web`, then `curl -fsS http://127.0.0.1:4187/`

## Acceptance Evidence

- Native store selection remains guarded by preflight checks for metadata,
  bd context, Dolt mode, identity, and version compatibility.
- Fallback remains available and explicit through `GC_BEADS_FORCE_FALLBACK=1`.
- Operator-facing status JSON includes beads store diagnostics without leaking
  credentials in preflight details.
- Generated OpenAPI/schema/dashboard types are in sync and pass dashboard
  build/typecheck.
- Native dependency closure is guarded by `scripts/check-native-dependency-surface.sh`.
