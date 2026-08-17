# Release gate: Git pathspec-checkout safety convention

- Deploy bead: `ga-pkz5av`
- Reviewed source: `6790090f180c15a40fd24fc94c6e770f3b6fa5a8`
- Source branch: `builder/ga-cm51rh` (provenance only)
- Base: `origin/main` at `d27aeadf46916ebc256c72df5131db0ea7e99876`
- Overall verdict: **PASS**

| # | Criterion | Verdict | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-2ggo42` records `REVIEWER VERDICT: PASS` against the exact reviewed SHA. |
| 2 | Acceptance criteria met | **PASS** | The diff adds one eight-line **Git safety** bullet immediately after **Tmux safety** in `AGENTS.md`. It names the destructive pathspec checkout, the safe `git show <ref>:<path>` read, and isolated-worktree alternatives. No script, hook, alias, or Go file changed. The required guidance was also mirrored to still-open bead `ga-ueq90`. |
| 3 | Tests pass | **PASS** | On an isolated checkout at the reviewed SHA: `make check-docs` passed; the same package rerun through `scripts/go-test-observable gate-docsync -- -count=1 ./test/docsync` recorded **13 PASS, 0 FAIL, 0 SKIP tests**; `make test-fast-parallel` recorded **10 PASS, 0 FAIL, 0 SKIP jobs**; `go vet ./...` passed. No skip justification is required. The `AGENTS.md`-only diff matches none of the optional process/integration path filters in `.github/workflows/ci.yml`. |
| 4 | No high-severity review findings open | **PASS** | The reviewer reported no issues; unresolved HIGH findings: **0**. |
| 5 | Final branch is clean | **PASS** | The isolated checkout reported zero status entries before and after the gate commands. |
| 6 | Branch diverges cleanly from main | **PASS** | After the gate began, `origin/main` advanced by one unrelated tmux commit. The final divergence is `1` base-only and `1` source-only from merge base `30df2e64db3afd11bd18b4fc2cdd61c20b061f69`. `git merge-tree --write-tree origin/main 6790090f180c15a40fd24fc94c6e770f3b6fa5a8` completed without conflicts and produced tree `3dfb270014a60069ca11dfbaf19a3935684a7840`. |
| 7 | Single feature theme | **PASS** | One contributor-guidance file changed for one Git worktree-safety convention. |

## Release decision

The change is ready for an isolated deploy branch and pull request.
