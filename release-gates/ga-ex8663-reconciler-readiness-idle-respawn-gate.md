# Release Gate: Reconciler graph-only readiness + idle-respawn

- Deploy bead: `ga-ex8663`
- Source bead: `ga-srtu0u`
- Review bead: `ga-0lgc4e`
- Branch: `builder/ga-srtu0u-graph-only-readiness-idle-respawn`
- Reviewed commit: `b3e98d2cb0e2c1f0ab29e1e2f52af9727a8f6a1e`
- PR: https://github.com/gastownhall/gascity/pull/3835
- Gate worktree: `/home/jaword/gascity-deploy-ga-ex8663-reconciler-gate`
- Gate date: 2026-06-30

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses
the release criteria from the deployer role prompt and `TESTING.md`.

## Scope

The branch contains two commits above `origin/main`:

- `bfa6a0a1f` - `fix(reconciler): graph-only readiness + idle-respawn safety-net`
- `b3e98d2cb` - `test(docsync): skip ga- agent work directories in TestDocDirCoverage`

The diff is one reconciler/readiness feature theme plus its docsync test
allowlist adjustment:

- `cmd/gc/build_desired_state.go`
- `cmd/gc/build_desired_state_graph_ready_test.go`
- `cmd/gc/session_idle_respawn_test.go`
- `cmd/gc/session_reconciler.go`
- `cmd/gc/session_wake.go`
- `internal/beads/ready_graph_only.go`
- `test/docsync/docsync_test.go`

## Checklist

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | `ga-0lgc4e` is closed with reviewer verdict PASS for branch `builder/ga-srtu0u-graph-only-readiness-idle-respawn`, commits `bfa6a0a1f` and `b3e98d2cb`. |
| 2 | Acceptance criteria met | PASS | The source bead required graph-only readiness for controller-demand awake gating and an idle-respawn safety net for pool sessions awake only for ready assigned work. Reviewer notes confirm the new graph-only probe, fallback behavior, idle-probe eligibility, non-cancelable `idle-respawn` drain reason, and focused tests. |
| 3 | Tests and required static checks pass | FAIL | GitHub PR #3835 preflight failed in a fresh checkout: `cmd/gc/session_reconciler.go:3826:6: QF1001: could apply De Morgan's law (staticcheck)`. The failure caused `Preflight / static checks`, `Check`, `CI / preflight`, and `CI / required` to fail on run `28438932407`. Local `make lint` also surfaced the same `QF1001` finding, though the local lint run was polluted by unrelated sibling-worktree findings. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list only minor non-blocking observations and no HIGH findings. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` was clean on `builder/ga-srtu0u-graph-only-readiness-idle-respawn`. |
| 6 | Branch diverges cleanly from main | PASS | PR #3835 reports `mergeable: MERGEABLE`. Local branch head is `b3e98d2cb`; `origin/main` is `1b5999531`; merge base is `fdfca60c`. |
| 7 | Single feature theme | PASS | The commit set touches one reconciler readiness/idle-respawn subsystem, with a small docsync test allowlist adjustment required by the repo's agent-worktree layout. |

## Blocking Failure

The branch cannot pass release gate while staticcheck rejects the condition in
`cmd/gc/session_reconciler.go`:

```text
cmd/gc/session_reconciler.go:3826:6: QF1001: could apply De Morgan's law (staticcheck)
        if !((len(eval.Reasons) == 0 && eval.ConfigSuppressed) || idleAssignedWorkOnly(eval)) {
           ^
```

## Decision

FAIL. Route `ga-ex8663` back to builder for the staticcheck fix. Do not push a
deployer gate commit or request merge until the branch clears required static
checks.
