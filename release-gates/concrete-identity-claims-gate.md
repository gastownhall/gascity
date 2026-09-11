# Release Gate: Concrete identity claims

Deploy bead: ga-2igrdw
Source review bead: ga-qlzkjn
Implementation bead: ga-98gjgb
Gate date: 2026-07-01

This deploy spans two repositories under one feature theme:

- `gc-management` local-only repo: commit
  `a7654c1364d48c2adf6e6b3690b005471aa252f0` on
  `builder/ga-98gjgb-concrete-identity-claims`.
- `gascity` SDK repo: commit
  `5bcabe3e638e90abb3ee8938b2389aec25b3e346` on
  `builder/ga-98gjgb-docs-convention`.

The `gc-management` repo has no git remote, so no PR can be opened there. This
gate is committed to the `gascity` docs PR branch and the local-only merge is
handed to mayor/mpr as a merge authority action.

Note: docs/PROJECT_MANIFEST.md is not present in this worktree. This gate uses
the deployer release criteria and the relevant repo guidance.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-qlzkjn is closed with `REVIEW VERDICT: PASS` for both branches. Deploy bead ga-2igrdw was created by reviewer-gm-u4aay after review PASS. |
| 2 | Acceptance criteria met | PASS | `gc-management`: 37 concrete-identity replacements are present; the remaining 14 `assignee="$GC_TEMPLATE"` matches are Tier-2 `bd ready --assignee` lines only. The irregular TOML formula parses with `tomllib`. `gascity`: docs add the claim identity convention and a cross-reference. |
| 3 | Tests pass | PASS | `gascity`: `make check-docs` passed. `gc-management`: `git diff --check main...HEAD` passed; `gc lint .` was run and exits nonzero only with pre-existing baseline categories also reproduced on a temporary detached `main` worktree. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no unresolved HIGH findings. The known post-merge runtime observation gap is documented as non-blocking and requires the prompt fix to be live. |
| 5 | Final branch is clean | PASS | `gascity` branch is clean apart from this committed gate file. `gc-management` feature worktree is clean. |
| 6 | Branch diverges cleanly from main | PASS | `gascity`: `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree ef21eaae42cd81f9c11fabdecdf42a70e266de11. `gc-management`: `git merge-tree --write-tree main HEAD` succeeded and produced tree 4d439b3e9c2afdf2c511b4e5f7d79b9715d2d3a4. |
| 7 | Single feature theme | PASS | Both branches implement the same claim-identity convention: Tier-1 recovery and claims use concrete session identity; shared Tier-2/Tier-3 queue discovery remains template-routed. |

## Acceptance Checks

- PASS: `grep -RInF 'assignee="${GC_ALIAS:-$GC_TEMPLATE}"' packs | wc -l`
  returned `37` in the `gc-management` feature worktree.
- PASS: `grep -RIn 'assignee="\$GC_TEMPLATE"' packs` returned exactly the 14
  expected Tier-2 `bd ready --assignee="$GC_TEMPLATE"` lines.
- PASS: `packs/actual/deployer/formulas/mol-deployer-gate.formula.toml`
  parsed successfully with Python `tomllib`.
- PASS: `gc-management` has no remote, confirmed by empty `git remote -v`.
- PASS: `gascity` docs changed only
  `engdocs/architecture/prompt-templates.md` and
  `engdocs/design/session-model-unification.md`.

## Commands

```text
# gascity SDK repo
git diff --check origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
make check-docs

# gc-management local-only repo
git diff --check main...HEAD
git merge-tree --write-tree main HEAD
grep -RIn 'assignee="\$GC_TEMPLATE"' packs | sort
grep -RInF 'assignee="${GC_ALIAS:-$GC_TEMPLATE}"' packs | wc -l
python3 - <<'PY'
import tomllib
from pathlib import Path
path = Path('packs/actual/deployer/formulas/mol-deployer-gate.formula.toml')
tomllib.loads(path.read_text())
print('toml ok')
PY
gc lint .
```

## Known Baseline

`gc lint .` in `gc-management` returns nonzero on this branch and on a temporary
detached `main` worktree. The reproduced baseline categories are named-session
plus pool coexistence warnings, missing shared prompt-template includes for
existing packs, and legacy PackV1 order-path warnings. The changed files did not
introduce new lint categories.
