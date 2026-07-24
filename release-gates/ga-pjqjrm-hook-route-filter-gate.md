# Release Gate: ga-pjqjrm hook route filter

Gate run: 2026-07-16T09:51:24Z

Deploy bead: `ga-fsizxp`

PR: https://github.com/gastownhall/gascity/pull/3952

Branch: `release/ga-pjqjrm-hook-route-filter`

Head: `83a5f603ca1e8cbebb2cd2e9d5fceb77ffac2544`

Base: `origin/main` at `d1b7c04262e44a4eaef160feafb6c74675991022`

Project manifest note: `docs/PROJECT_MANIFEST.md` and `PROJECT_MANIFEST.md`
are not present in this checkout. This gate uses the active deployer release
criteria and the repository testing guidance in `TESTING.md`.

## Summary

This is a single-bead deploy for PR #3952, which routes plain `gc hook`
display output through the same claim route filtering predicates already used
by `gc hook --claim`.

The branch also carries the supporting `work_query_unfiltered` config opt-out,
config patch/override/migration wiring, generated schemas and OpenAPI/client
artifacts, and the resource-census baseline update required by the new tests.

## Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Reviewer bead `ga-cfys3t` is closed with close reason `pass`; notes contain `Review verdict: PASS (gascity/reviewer)` and `No blocking findings.` |
| 2 | Acceptance criteria met | PASS | The diff adds `filterHookCandidatesByRoute` / `hookCandidateVisibleForDisplay` and applies it to the no-`--claim` hook display path. It preserves assigned-to-self visibility, keeps unrouted candidates for the active run target visible, and adds `work_query_unfiltered` as the opt-out for intentionally broad custom work queries. Tests and config/schema/migration wiring cover those surfaces. |
| 3 | Tests pass | PASS | Local: `make test-fast-parallel` passed all 8 fast jobs; `go vet ./...` clean; `make dashboard-check` passed; targeted `go test ./cmd/gc ./internal/config ./internal/migrate -run 'TestCmdHookDisplayFiltersByRoute|TestCmdHookDisplayRouteFilterOptOutWithWorkQueryUnfiltered|TestAgentFieldSync|TestApplyAgentPatchCoversAllFields|TestApplyAgentOverrideCoversAllFields|TestAgentCloneIsDeep|TestMigrate' -count=1` passed. GitHub status check rollup for PR #3952 at this head has required checks passing. |
| 4 | No high-severity review findings open | PASS | Reviewer notes explicitly state no blocking findings. No unresolved HIGH finding is recorded in `ga-fsizxp` or `ga-cfys3t`. |
| 5 | Final branch is clean | PASS | Scratch worktree was clean before writing this gate; this gate file is the only deployer change and is committed as the release-gate PASS commit. `git status` is rechecked clean before push. |
| 6 | Branch diverges cleanly from main | PASS | After refresh, `git merge-base origin/main HEAD` is `d1b7c04262e44a4eaef160feafb6c74675991022`; `git rev-list --left-right --count origin/main...HEAD` reports `0 5`; `git merge-tree --write-tree origin/main HEAD` succeeds with tree `3c114209e7e146868ed14a7cc7278e8e846e25d9`. `gh pr view 3952` reports `mergeable=MERGEABLE`, `mergeStateStatus=CLEAN`, head OID `83a5f603ca1e8cbebb2cd2e9d5fceb77ffac2544`. |
| 7 | Single feature theme | PASS | `git diff --name-only origin/main..HEAD` is confined to the hook display route-filter feature and required support: `cmd/gc` hook tests/logic, config patch/override/migration/pool copy wiring, generated schema/OpenAPI/dashboard client files, resource-census/test guidance updates, and this gate file. No independent subsystem or user-facing feature is bundled. |

## Acceptance Checks

- Plain `gc hook <agent>` display uses the same identity and route predicates
  as the claim path.
- Assigned-to-self work remains visible for crash-recovery pickup.
- Unrouted workflow-root candidates for the active run target remain visible.
- `work_query_unfiltered` keeps intentionally broad custom work queries
  available without weakening `--claim` enforcement.
- `config.Agent`, `AgentPatch`, `AgentOverride`, patch/override application,
  pool deep-copy, migration types, OpenAPI, generated clients, and reference
  schemas were updated together.

## Test Evidence

```text
HOME=/home/jaword TMPDIR=/var/tmp/gp make test-fast-parallel
All fast jobs passed

HOME=/home/jaword TMPDIR=/var/tmp/gp go vet ./...
PASS

HOME=/home/jaword TMPDIR=/var/tmp/gp make dashboard-check
PASS

HOME=/home/jaword TMPDIR=/var/tmp/gp go test ./cmd/gc ./internal/config ./internal/migrate \
  -run 'TestCmdHookDisplayFiltersByRoute|TestCmdHookDisplayRouteFilterOptOutWithWorkQueryUnfiltered|TestAgentFieldSync|TestApplyAgentPatchCoversAllFields|TestApplyAgentOverrideCoversAllFields|TestAgentCloneIsDeep|TestMigrate' \
  -count=1
PASS
```

## Decision

PASS. All seven release criteria hold for PR #3952 at
`83a5f603ca1e8cbebb2cd2e9d5fceb77ffac2544`. Route merge-request to mayor/mpr;
the deployer does not merge.
