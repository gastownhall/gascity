# Release Gate: prune dead-letter nudges after backing-bead reaping

- Deploy bead: `ga-j250d0`
- Build bead: `ga-18w8e5`
- Review bead: `ga-osffxo`
- Reviewed source: `48c0eb6cf380a6e0e075340b189e93748c532cd8`
- Provenance branch: `builder/ga-ev4kfb` (not a push target)
- Base checked: `origin/main@5bbbb643f8fee5b2736e534eae879158e9f6facd`
- Evaluated: 2026-08-16
- Verdict: **PASS**

The deploy bead's original description still names the previously rejected
commit `544b16526d2f505a67d14cd7ef98b7111e2f5258`. The resubmission review,
typed `metadata.commit`, reviewer mail, and reviewer verdict all supersede it
with the git-resolved commit above. The new two-commit stack is content-identical
to the original review and adds the required `(refs ga-18w8e5)` provenance to
both subjects.

`docs/PROJECT_MANIFEST.md` is absent from this checkout and from current
`origin/main`; this checklist applies the active deployer release criteria and
the repository testing policy in `TESTING.md`.

## Gate checklist

Criterion 6 was evaluated first, after the already-merged pre-flight found no
pull request carrying the reviewed source.

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | **PASS** | Review bead `ga-osffxo` is closed with reason `pass`; its verdict step `ga-a21008` records `VERDICT: PASS` for the exact reviewed source. All five review steps are closed. |
| 2 | Acceptance criteria met | **PASS** | Code inspection confirms that successful `Terminalize` is sufficient to prune an expired dead-letter entry even when its backing bead was already reaped. `Terminalize` explicitly treats a missing bead as a nil-error no-op. The new regression test proves the reaped-bead case, and the two latency/budget tests prove the reduced three-store-operation repair path and correct survivor accounting. The reviewer's spec step reports the required issue directive covered, optional considerations unchanged, and no uncovered criteria. |
| 3 | Tests pass | **PASS** | The required process gate passed all 7 job scopes. The fast gate passed 9 of 10 job scopes; its sole failure, `TestProviderLiveClaudeKindPath`, is attributable under criterion 3a as documented below. All three diff-owned tests executed by name and passed with zero skips. Build, full lint, format, vet, and the CI policy lane passed. |
| 4 | No high-severity review findings open | **PASS** | Review steps report `style_findings: none`, no OWASP-relevant surface, no uncovered criteria, and a final PASS. Unresolved HIGH count: `0`. |
| 5 | Final branch is clean | **PASS** | The reviewed source was tested in isolated worktree `/var/tmp/ga-j250d0-gate.fcoXlB/repo`; `git status --short --branch` reported only detached HEAD and no changes before this checklist was added. `git diff --check origin/main...48c0eb6c` produced no output. Repository hooks resolve to `.githooks`. |
| 6 | Branch diverges cleanly from main | **PASS** | `git merge-tree --write-tree origin/main 48c0eb6cf380a6e0e075340b189e93748c532cd8` exited 0 and produced tree `4401e14821181eff58b24179917bd05e591342e5`; no self-rebase was needed. The reviewed SHA and both parent commits resolve in Git. |
| 7 | Single feature theme | **PASS** | The two-commit TDD stack changes four files under `cmd/gc`, all within nudge dead-letter cleanup: the pruning behavior, its new regression test, and the two budget/backlog tests updated for the reduced store-operation count. No independent feature is bundled. |

The mandatory ancestry-scope guard also passed before branch creation:

```text
assert_deploy_ancestry_scope origin/main 48c0eb6cf380a6e0e075340b189e93748c532cd8 ga-j250d0 ga-18w8e5
exit 0
```

## Test evidence

Environment setup before test execution:

- `DOCKER_HOST=unix:///run/user/1000/podman/podman.sock`
- `TESTCONTAINERS_RYUK_DISABLED=true`
- Rootless Podman 5.8.4 and its socket were confirmed available.

Commands and counts:

- `make test-cmd-gc-process-parallel`: **7 PASS job scopes, 0 FAIL, 0 SKIP** (six process shards plus `productmetrics-testhook`).
- `make test-fast-parallel`: **9 PASS job scopes, 1 attributed FAIL, 0 SKIP**; all six `cmd/gc` shards passed.
- Focused diff-owned run: **3 PASS, 0 FAIL, 0 SKIP**.
- `make test-ci-policy`: **PASS**.
- `go build ./...`: **PASS**.
- `make lint`: **PASS**, 0 issues.
- `make fmt-check`: **PASS**, no diff.
- `go vet ./...`: **PASS**.

`diff_tests_executed`:

- `TestPruneDeadQueuedNudges_PrunesItemsWhoseBeadWasReaped`: PASS
- `TestSlingNudgeEnqueueBoundedByBacklog`: PASS
- `TestSlingNudgeEnqueueBudgetPreservesQueuedItems`: PASS

`test_counts`: required job gates **16 PASS / 1 attributed FAIL / 0 SKIP**;
diff-owned tests **3 PASS / 0 FAIL / 0 SKIP**.

`skip_justification`: not applicable — zero skips.

`waiver_ref`: none required.

`policy_lane`: `make test-ci-policy` — PASS.

`failure_attribution`:

- `TestProviderLiveClaudeKindPath` -> `ga-cqq3hs.1`. The failing test is in
  `internal/runtime/herdr/kindpath_live_test.go`, while this diff touches only
  four `cmd/gc` nudge files. A focused run at current
  `origin/main@5bbbb643f8fee5b2736e534eae879158e9f6facd` failed with the same
  `agent_pane_busy` / `w1:p1 is not an available shell` signature. It is not
  diff-owned, has a live tracker, reproduces on the base, and has no path
  overlap, satisfying all four criterion-3a clauses.

## Gate decision

The reviewed change is conflict-free with current main, has complete
diff-owned execution evidence, and introduces no un-attributed test failure.
It is eligible for an isolated deploy branch and pull request.
