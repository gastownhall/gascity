# Release Gate: ga-zron27 - herdr kind-path name isolation

Deploy bead: `ga-zron27`  
Review bead: `ga-hmd2gu`  
Source build bead: `ga-fh1flg`  
Reviewed content commit: `2ec870e236eb32fef0fa7af56281981b34f2afce`  
Evaluated rebased commit: `62ee9f212bc33051f6e1e1a9375e86bee0e1db72`  
Local deploy branch: `deploy/ga-zron27-gate-r3-20260820`  
Base: `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`  
Gate evaluated: 2026-08-20  
Verdict: **PASS — MAYOR WAIVER APPLIED**

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses
the release criteria in the deployer prompt together with `TESTING.md` and
`engdocs/contributors/release-gate-criteria-conventions.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Closed review bead `ga-hmd2gu` records round-2 `REVIEW VERDICT: PASS`. `git diff --exit-code 2ec870e236eb32fef0fa7af56281981b34f2afce 62ee9f212bc33051f6e1e1a9375e86bee0e1db72 -- internal/runtime/herdr/kindpath_live_test.go internal/runtime/herdr/panebinding_provider_test.go` returned 0, proving the rebased candidate's reviewed content is byte-identical. |
| 2 | Acceptance criteria met | PASS | The PID-plus-atomic-counter name generator and both required hermetic proofs are present. `TestKindPathNamesAreUnique` and `TestKindPathNamesWorkThroughFakeProviderLifecycle` both passed in a fresh focused run. |
| 3 | Tests pass | **PASS — WAIVED** | The guarded push's `make test-fast-parallel` run completed 9 PASS jobs / 1 FAIL job. The diff-owned `TestProviderLiveClaudeKindPath` returned `agent_pane_busy` for unique agent `kindsmoke-3319807-4` targeting pane `w1:p1`; it remains preserved as **FAIL — WAIVED**. The two hermetic diff-owned tests passed. The prior exact reviewed-content full union completed 33 PASS / 7 FAIL jobs, with this same live-test signature as the only diff-owned failure; a subsequent unchanged-content 40-job run reported the `internal/runtime/herdr` package green twice and no `agent_pane_busy` occurrence. Mayor granted a bead-, test-, and mechanism-specific waiver after independently confirming the same pane-readiness failure on the untouched sibling `TestProviderLive`. `waiver_ref: mayor-2026-08-20-ga-zron27-c3` (granted by mayor; audited on `ga-zron27.2` and mail `gm-wisp-l7c8y2`). |
| 4 | No high-severity review findings open | PASS | The round-2 review verdict is PASS with no unresolved HIGH finding. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean on the rebased deploy branch before this gate record was written. No source edits were made by deployer. |
| 6 | Branch diverges cleanly from main | PASS | The bounded replay completed without conflict. `git merge-base --is-ancestor origin/main 62ee9f212bc33051f6e1e1a9375e86bee0e1db72` returned 0 and the merge base is exactly `origin/main@7c817e0640fae801631043005f1d54b17ce3e97c`. The subsequent guarded push was rejected because its pre-push test gate failed, so no remote deploy branch was created. |
| 7 | Single feature theme | PASS | The three rebased commits touch only two `internal/runtime/herdr` test files and one theme: isolating kind-path lifecycle test names and proving those names through the provider lifecycle. |

## Criterion 3 evidence

The pre-push fast gate wrote logs to `/var/tmp/gc-local-tests.bdq9Kc`.
`unit-core.log` records:

```text
--- FAIL: TestProviderLiveClaudeKindPath (0.33s)
kindpath_live_test.go:127: Start: herdr: start "kindsmoke-3319807-4":
agent_pane_busy: agent target pane w1:p1 is not an available shell
```

Focused hermetic verification on the same rebased SHA passed:

```text
TestKindPathNamesAreUnique: PASS
TestKindPathNamesWorkThroughFakeProviderLifecycle: PASS
```

This is the same lower-level pane-availability failure recorded in the prior
gate and tracked by `ga-nqlb8q`. The modified live test itself failed, so the
non-diff-owned attribution protocol does not apply. Instead, mayor supplied
the explicit criterion-3 waiver required by the prior disposition.

## Mayor waiver

Mayor granted `mayor-2026-08-20-ga-zron27-c3` specifically for
`TestProviderLiveClaudeKindPath` failing as `agent_pane_busy` / pane-not-ready
on this bead. The ruling does not dispute that the test is diff-owned. It is
based on mechanism proof outside the diff: reviewer `ga-hmd2gu` reproduced
the identical failure on untouched sibling `TestProviderLive`, while the two
hermetic tests prove PID-plus-counter names are unique and traverse the real
provider lifecycle shape.

The scope is deliberately narrow. Any other test, failure mechanism, or bead
requires normal attribution or a separate waiver. The raw fast result remains
9 PASS / 1 FAIL; this record does not rewrite it green.

## Disposition

- The explicit merge-authority waiver now satisfies the prior disposition.
- Push the isolated deploy branch with this preserved record and open its PR.
- Per the mayor's current standing rule, do not mail or route a merge request
  until maintainer-pr-review has produced a clear artifact for that PR.
- The deployer does not merge.
