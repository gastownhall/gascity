# Release Gate: review-formulas shallow-fetch race

Bead: `ga-gfwkdn`
Source review bead: `ga-fmqxhd`
PR: https://github.com/gastownhall/gascity/pull/3227
Branch: `fix/review-formulas-routing-shallow-race`
Commit under review: `1c32071455cbcd454d0c582bdb43359ca6c58d53`
Gate run: 2026-06-07 17:16 PDT (-0700)

## Summary

PASS. The branch makes the `dorny/paths-filter` step in the
`review-formulas routing` workflow non-fatal, preventing a transient shallow
checkout race from failing the routing gate. The filter output remains cosmetic:
the run/skip decision still depends only on event type and the
`needs-review-formulas` label.

`docs/PROJECT_MANIFEST.md` is not present in this worktree, so this gate uses
the release criteria supplied to the deployer role and the repository testing
rules in `TESTING.md`.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-fmqxhd` is closed with `REVIEW VERDICT: PASS (gascity/reviewer, 2026-06-08)`. |
| 2 | Acceptance criteria met | PASS | The workflow now sets `continue-on-error: true` only on the pinned `dorny/paths-filter` step; the routing script still makes push, workflow_dispatch, label, and draft decisions without using `PATH_HIT` for control flow. |
| 3 | Tests pass | PASS | Local: `make test-fast-parallel` passed all fast shards; `go vet ./...` completed cleanly. Remote: `gh pr checks 3227` shows all non-skipped checks passing, including `review-formulas routing`, `Integration / review-formulas`, `CI / required`, CodeQL, preflight, dashboard, integration shards, and worker-core gates. |
| 4 | No high-severity review findings open | PASS | Review notes list one INFO cosmetic observation only; no HIGH, CRITICAL, FAIL, or request-changes findings are present. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before adding this gate file; final clean status is rechecked after the gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` returned a tree (`ec7a8cae6d6d3cd754e243764907d4cd671488d9`) with no conflicts; `git diff --check origin/main..HEAD` reported no whitespace errors. |
| 7 | Single feature theme | PASS | The commit touches only `.github/workflows/review-formulas.yml` and changes one CI-routing failure mode. |

## Acceptance Criteria Evidence

| Acceptance criterion | Result | Evidence |
|---------------------|--------|----------|
| Tolerate transient shallow-fetch failure from `dorny/paths-filter` | PASS | `.github/workflows/review-formulas.yml` adds `continue-on-error: true` to the `dorny/paths-filter` step. |
| Preserve review-formulas run/skip behavior | PASS | The `Decide whether shard should run` step still sets `run_shard=true` for `workflow_dispatch`, `push`, and PRs with `needs-review-formulas`; PRs without the label still skip. `PATH_HIT` is used only inside the human-readable reason string. |
| Preserve security posture | PASS | The action remains pinned to `fbd0ab8f3e69293af611ebaee6363fc25e6d187d`; checkout still uses `persist-credentials: false`; the filter output does not affect permissions, secrets, or access decisions. |
| Keep scope narrow | PASS | `git diff --name-status origin/main..HEAD` lists only `.github/workflows/review-formulas.yml`. |

## Changed Files Reviewed

- `.github/workflows/review-formulas.yml`

## Commands Run

```text
gc hook gascity/deployer
bd show ga-gfwkdn
bd show ga-fmqxhd
gh auth status
gh pr view 3227 --json number,title,state,url,headRefName,headRepositoryOwner,baseRefName,mergeStateStatus,statusCheckRollup,reviewDecision,latestReviews
git fetch origin main fix/review-formulas-routing-shallow-race --prune
git diff --check origin/main..origin/fix/review-formulas-routing-shallow-race
git merge-tree --write-tree origin/main HEAD
make test-fast-parallel
go vet ./...
gh pr checks 3227 --repo gastownhall/gascity
```

## Notes

- PR #3227 already existed before deployer evaluation, so this deploy updates
  the existing PR branch with the gate evidence instead of creating a duplicate
  pull request.
- Mayor has autonomous mpr merges paused city-wide per the deploy bead; this
  gate routes a human merge-request to mayor and does not merge.
