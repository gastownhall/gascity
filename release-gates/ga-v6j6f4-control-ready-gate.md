# Release Gate: ga-v6j6f4 control-ready dispatch

Date: 2026-07-14

Branch under gate: `origin/deploy/ga-v6j6f4.1-control-ready-clean`

Gate worktree: `/var/tmp/gascity-deploy-ga-v6j6f4-current.ZdtHzK`

Head under gate: `814adbd4722d9c13fa59085c9c0c521e5ee609aa`

Base checked: `origin/main` at `a4046035b08e9f18f3e761371440da40cd70b12a`

Release criteria source: `docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses the release criteria from the active deployer prompt loaded by `gc prime`, plus `TESTING.md`.

## Change Summary

This branch keeps the control-dispatcher readiness scan from fork-execing `bd` once per candidate/route on every tick. `nextWorkflowServeBeads` now recognizes the existing control-ready query shape and answers it from an in-process `CachingStore` snapshot when possible. If the cache cannot answer, the fallback is a single batched `bd ready --json` call followed by the same Go-side candidate/route filtering.

The branch also carries a dedicated test-policy baseline bump for the new `t.Setenv` calls in the control-ready dispatch tests. The bump updates the three files that resourcecensus keeps in sync: `internal/testpolicy/resourcecensus/census.go`, `test/test-resources.toml`, and `TESTING.md`.

## Commit Set

| Commit | Summary |
| --- | --- |
| `5be4a4135` | `fix(dispatch): serve control-dispatcher readiness from CachedReady instead of per-agent bd fork-execs` |
| `456a46b84` | `fix(dispatch): raise control-ready fallback limit and log truncation (ga-bbj6wv)` |
| `814adbd47` | `test(resourcecensus): bump environment baseline for control-ready dispatch tests (ga-v6j6f4.1)` |

## Diff Scope

`git diff --name-status origin/main...HEAD`:

```text
M	TESTING.md
A	cmd/gc/dispatch_control_ready.go
A	cmd/gc/dispatch_control_ready_test.go
M	cmd/gc/dispatch_runtime.go
M	internal/beads/query.go
M	internal/testpolicy/resourcecensus/census.go
M	test/test-resources.toml
```

The previous gate failed criterion 7 because the original branch also carried unrelated ReadyGraphOnly/beads commits. This isolated branch does not touch those rejected-scope files.

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 6 | Branch diverges cleanly from main | PASS | Evaluated first. `git merge-tree --write-tree origin/main HEAD` returned exit 0 and wrote tree `9593583f15953dafe3a34b07ebb15e10286b7568`; no merge conflicts with current `origin/main`. |
| 1 | Review PASS present | PASS | Review bead `ga-bbj6wv` is closed with `Reviewer verdict: PASS (re-review)`. The reviewer confirmed Finding 1 fixed by the fallback limit/logging change and Finding 2 addressed by documentation. |
| 2 | Acceptance criteria met | PASS | `ga-ak6rt1` done-when is met: clean-cache readiness is served from `CachedReady`, dirty/unprimed compatibility uses one batched fallback call, candidate precedence and legacy/bare routing are covered by tests, and `rg -n 'Mayor|Deacon|Polecat|mayor|deacon|polecat'` over touched Go files returned no matches. |
| 3 | Tests pass | PASS | First `TMPDIR=/var/tmp GOTMPDIR=/var/tmp make test-fast-parallel` failed only `TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill` with `async starts did not finish`; exact subtest rerun passed. Second full `TMPDIR=/var/tmp GOTMPDIR=/var/tmp make test-fast-parallel` passed all fast jobs. `TMPDIR=/var/tmp GOTMPDIR=/var/tmp go vet ./...` also passed. |
| 4 | No high-severity review findings open | PASS | Review notes list one Medium and one Low finding; both are resolved in the PASS re-review. No HIGH finding appears in `ga-bbj6wv` or `ga-v6j6f4.3` notes. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` returned only `## HEAD (no branch)`. This checklist is the only deployer-created file and will be committed as the final branch tip before PR creation. |
| 7 | Single feature theme | PASS | Commit set is one feature theme: control-dispatcher readiness source and directly supporting tests/resourcecensus baselines. The diff is limited to `cmd/gc` dispatch readiness, `internal/beads` ready ordering export, and synchronized test policy baseline files. |

## Test Evidence

```text
TMPDIR=/var/tmp GOTMPDIR=/var/tmp make test-fast-parallel
first run: FAIL, unit-cmd-gc-4-of-6 only
failure: TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates/named_session_post-kill: async starts did not finish

TMPDIR=/var/tmp GOTMPDIR=/var/tmp go test ./cmd/gc -run '^TestSessionReconcilerTraceGH1654WorkRequestedStartCandidates$/^named_session_post-kill$' -count=1 -v
PASS

TMPDIR=/var/tmp GOTMPDIR=/var/tmp make test-fast-parallel
All fast jobs passed

TMPDIR=/var/tmp GOTMPDIR=/var/tmp go vet ./...
PASS
```

## Gate Result

PASS. Open a PR from `deploy/ga-v6j6f4.1-control-ready-clean` and route the merge-request to mayor. Deployer must not merge.
