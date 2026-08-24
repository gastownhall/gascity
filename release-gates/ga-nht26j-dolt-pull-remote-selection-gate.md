# Release gate: deterministic Dolt pull remote selection

- Deploy bead: `ga-nht26j`
- Build/review work: `ga-fe5cva` / `ga-mdce9d` / `ga-g04htm`
- Reviewed commit: `48f9a083c2f83dc5a481455361714625ff4471f5`
- Base: `origin/main@3f3af4a78419d93b2fafea1a514e9c875ddb7200`
- Deploy mode: remote; resolved push target: `origin`
- Evaluated: 2026-08-24
- Verdict: **FAIL** — no deploy branch was cut, nothing was pushed, and no pull request was opened

## Gate checklist

The target-already-merged pre-flight found no pull request carrying the reviewed
commit. Criterion 6 was then evaluated first, as required. It failed, and the
bounded self-rebase helper could not resolve the conflicts, so all remaining
criteria were skipped fail-fast.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **SKIPPED** | Not re-evaluated after criterion 6 failed. The bead still records the reviewer PASS pinned to the reviewed commit. |
| 2 | Acceptance criteria met | **SKIPPED** | Not re-evaluated after criterion 6 failed. |
| 3 | Tests pass | **SKIPPED** | The required test union was deliberately not run on a source that cannot merge cleanly into current main. |
| 3a | Pre-existing failures attributed | **SKIPPED** | No current test run was performed. |
| 3b | Policy and static lanes | **SKIPPED** | No current policy/static result is asserted. |
| 4 | No high-severity review findings open | **SKIPPED** | Not re-evaluated after criterion 6 failed. |
| 5 | Final branch is clean | **SKIPPED** | Not scored after criterion 6 failed; the helper nevertheless verified a clean tree before attempting the rebase and restored the source branch cleanly on failure. |
| 6 | Branch diverges cleanly from main | **FAIL** | `git merge-tree --write-tree --messages origin/main 48f9a083c2f83dc5a481455361714625ff4471f5` exited 1. Conflicts are in `TESTING.md`, `internal/testpolicy/resourcecensus/census.go`, and `test/test-resources.toml`. The mandated `attempt_bounded_self_rebase builder/ga-nht26j-footprint-fix main` returned `rc=12` for non-trivial conflicts and restored the branch to the reviewed SHA. |
| 7 | Single feature theme | **SKIPPED** | Not re-evaluated after criterion 6 failed. |

## Pre-flight and provenance evidence

- `gh api repos/gastownhall/gascity/commits/48f9a083c2f83dc5a481455361714625ff4471f5/pulls` returned no pull request.
- The reviewed commit is reachable from `origin/builder/ga-nht26j-footprint-fix`, and that remote ref resolves to the exact reviewed SHA.
- `assert_deploy_ancestry_scope origin/main 48f9a083c2f83dc5a481455361714625ff4471f5 ga-nht26j ga-mdce9d ga-fe5cva` passed before the rebase attempt.
- `git push --dry-run origin HEAD` succeeded, so the bounded helper correctly used `PUSH_REMOTE=origin`.
- After `rc=12`, `builder/ga-nht26j-footprint-fix` remained clean at `48f9a083c2f83dc5a481455361714625ff4471f5` and still matched its remote-tracking ref.

## Failed-gate disposition

The reviewed source is not releasable against current main. The builder must
rebase the existing `builder/ga-nht26j-footprint-fix` branch, resolve the three
policy-ledger conflicts coherently, run the affected checks, push the rebased
branch, and obtain a fresh reviewer PASS for the new SHA before the deploy gate
can be attempted again.
