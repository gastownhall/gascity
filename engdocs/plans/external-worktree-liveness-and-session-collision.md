# Plan: Make externally created work visible to session orchestration

> Owner: `gascity/pm` - Created: 2026-08-07
> Source: `ga-1xaqgo`
> Architecture source: `gascity/architect`, validated against `origin/main`
> Decomposed into: three `ready-to-build` bug fixes

## Context

Gas City can start a duplicate session when work is already active inside a
worktree created by Claude Code's `EnterWorktree` flow. Three confirmed gaps
combine to produce the collision:

1. Worktree liveness discovery discards registered worktrees outside the
   gc-owned `.gc/worktrees` root, including `.claude/worktrees`.
2. Plain `gc hook` output does not remove already-assigned candidates, and the
   related foreign-route fallback filter remains unmerged with a duplicate
   route-matching implementation.
3. Reconciler capacity counts only sessions gc created, so externally active
   work does not occupy the configured agent slot.

The root bead is authoritative over the earlier `crn-7vrp` analysis where its
citations or conclusions differ. Tracker import was a no-op because the
`tracker-to-beads` bridge is not installed in this PM worktree.

## User outcome

When a process is already working in a registered Git worktree, Gas City can
see that activity and avoid starting duplicate work for the same configured
slot. Visibility does not grant deletion authority: gc never reaps or
terminates a worktree or process it did not create.

## Work packages

| ID | User story | Routing | Depends on |
| --- | --- | --- | --- |
| `ga-1xaqgo.1` | As an operator, all live Git worktrees are visible without allowing gc to reap foreign worktrees | `ready-to-build` -> `gascity/builder` | - |
| `ga-1xaqgo.2` | As an agent, `gc hook` hides already-assigned and foreign-routed beads consistently | `ready-to-build` -> `gascity/builder` | - |
| `ga-1xaqgo.3` | As an operator, reconciler capacity accounts for live work in externally created worktrees | `ready-to-build` -> `gascity/builder` | `ga-1xaqgo.1` |

## Acceptance rollup

The downstream work is complete when:

- A live process under a registered `.claude/worktrees/...` worktree appears
  in the shared worktree-liveness result even though it is outside the
  gc-owned root.
- Reaper ownership gates remain unchanged in effect: foreign worktrees and
  processes are visible but are never deleted, killed, or auto-terminated.
- Plain `gc hook` excludes non-empty assignees using the same eligibility fact
  as the claim path.
- The proven foreign-route fallback work from `f9ea2a563c` is ported without
  retaining a second route-spelling predicate; display and claim paths share
  one matcher and the same alias semantics.
- Reconciler capacity consumes the widened liveness result rather than running
  another worktree/process scan.
- With `max_active_sessions = 1`, matching externally active work prevents a
  duplicate session, while inactive worktrees and unrelated qualified agent
  configs do not suppress legitimate demand.
- Work-directory correlation honors both `gc.work_dir` and the legacy
  `work_dir` key written by the reconciler.
- Each fix follows RED/GREEN TDD, keeps each risk at its smallest owning test
  layer, uses no fixed sleeps or open-coded polling, and passes its focused
  tests, the affected sharded suite, `make test-fast-parallel`, and
  `go vet ./...`.

## Dependency graph

```text
ga-1xaqgo.1  widen liveness discovery and preserve reaper ownership
       |
       v
ga-1xaqgo.3  count externally active work in reconciler capacity

ga-1xaqgo.2  align hook display, claim eligibility, and route matching
```

Cause B is independent of worktree discovery and can proceed in parallel.
Cause C is blocked by cause A because it must consume A's shared liveness
result rather than introduce a second scan.

## Routing rationale

The architect validated all three causes and their safety guardrails, so no
child needs another architecture or UX pass. Each package routes directly to
the builder and carries its own test-first acceptance criteria. The packages
stay separate because they own different regressions and because cause C has a
real dependency on cause A's output boundary.

## Risks

- Widening discovery and widening deletion authority together could destroy a
  foreign worktree. Tests must keep those scopes separate.
- A second process/worktree scan in capacity accounting could drift from the
  reaper's liveness view and recreate the original defect under a new spelling.
- Porting `f9ea2a563c` unchanged would add a second route matcher whose alias
  behavior differs from the claim path.
- An over-broad capacity match could suppress unrelated qualified agent
  configs, contrary to the per-config capacity contract.
- Tests that wait on elapsed time could turn a concurrency fix into a flaky
  gate; synchronization must wait on observable state.

## Out of scope

- Auto-terminating, adopting, or deleting externally created sessions or
  worktrees.
- Turning `max_active_sessions` into a fleet-wide singleton cap.
- Adding role-specific exceptions, status files, or process-local state as the
  source of truth.
- Rebuilding the unmerged hook filter from scratch without first using
  `f9ea2a563c` as archaeology evidence.
- Dashboard, API-schema, or public documentation changes unless implementation
  reveals a separately approved user-facing requirement.
