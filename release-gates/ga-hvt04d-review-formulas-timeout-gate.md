# Release Gate: ga-hvt04d review-formulas timeout

Bead: ga-hvt04d  
Source review bead: ga-xx5xan  
Reviewed commit: 40de01b5658fb1ce00056df3e16bbba6355c47ba  
Feature branch: builder/ga-ftgzxd  
Gate date: 2026-06-19

## Scope

Single-bead deploy for a CI workflow timeout fix. The reviewed change updates
`.github/workflows/review-formulas.yml` so the `review-formulas-shard` job has
`timeout-minutes: 45` instead of `30`.

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the release criteria from the deployer prompt and the bead's explicit
build/smoke instruction.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-xx5xan` reports `REVIEWER VERDICT: PASS (2026-06-19T11:15:12Z)` for commit `40de01b5658fb1ce00056df3e16bbba6355c47ba`. |
| 2 | Acceptance criteria met | PASS | `git diff origin/main..HEAD -- .github/workflows/review-formulas.yml` shows exactly one behavior change: `timeout-minutes: 30` -> `timeout-minutes: 45` for `review-formulas-shard`. This restores headroom above the 24 minute internal shard timeout described in the bead. |
| 3 | Tests pass | PASS | `make build` passed, producing `bin/gc` from commit `40de01b56`; `python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/review-formulas.yml"))'` passed; `./scripts/test-integration-shard review-formulas-basic-2-of-2` passed with `ok github.com/gastownhall/gascity/test/integration 104.569s`. |
| 4 | No high-severity review findings open | PASS | Review notes for `ga-xx5xan` list no findings: style clean, security no impact, coverage not required for a CI config change, confidence HIGH. |
| 5 | Final branch is clean | PASS | After committing the gate file, `git status --short --branch` reported `## HEAD (no branch)` with no modified or untracked files. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree --messages origin/main HEAD` returned merged tree `cf3899d332447c76432fe453101a863226c59d32` with no conflict messages. |
| 7 | Single feature theme | PASS | Commit set touches only `.github/workflows/review-formulas.yml` and changes one CI job timeout for the review-formulas shard. |

## Commands Run

```text
bd show ga-hvt04d
bd show ga-xx5xan
git fetch origin main
git fetch fork builder/ga-ftgzxd
git show --stat --oneline 40de01b5658fb1ce00056df3e16bbba6355c47ba
git diff --name-status origin/main..40de01b5658fb1ce00056df3e16bbba6355c47ba
git merge-tree --write-tree --messages origin/main 40de01b5658fb1ce00056df3e16bbba6355c47ba
make build
python3 -c 'import yaml; yaml.safe_load(open(".github/workflows/review-formulas.yml")); print("workflow yaml parse ok")'
./scripts/test-integration-shard review-formulas-basic-2-of-2
git diff --check origin/main..HEAD
git status --short --branch
git merge-tree --write-tree --messages origin/main HEAD
```

## Result

PASS. Proceed to push the feature branch, open a PR, record the PR URL on
`ga-hvt04d`, close the deploy bead, and route a merge request to mayor.
