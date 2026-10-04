# Plan: partial scale_check create gate release

> Owner: `gascity/pm` - Created: 2026-06-23
> Sources: designer handoff `ga-0xljyj`; architecture root `ga-01yukx`;
> prior PM plan `engdocs/plans/fail-closed-pool-create-partial-demand.md`

## Goal

Release the completed `builder/ga-0xljyj-scale-check-partial-gate` branch
through review, deploy gating, and one post-merge verification loop.

This is not a new implementation breakdown. The broader A/B/C implementation
track already exists under `ga-4qbgqf.*`; `ga-0xljyj` is the PM intake for the
completed branch and release path.

## Work breakdown

| Bead | Title | Routes to | Gate |
|------|-------|-----------|------|
| `ga-0xljyj.1` | Review partial scale_check create-gate branch before release | `gascity/reviewer` | `needs-review` |
| `ga-0xljyj.2` | Gate and PR the partial scale_check create-gate branch | `gascity/deployer` | `needs-deploy` |
| `ga-0xljyj.3` | Monitor merged partial scale_check create gate in one reconciler cycle | `gascity/deployer` | `needs-deploy` |

## Dependency graph

```text
ga-0xljyj.1
  -> blocks ga-0xljyj.2
       -> blocks ga-0xljyj.3
```

## Acceptance summary

### `ga-0xljyj.1`

1. Review `builder/ga-0xljyj-scale-check-partial-gate` against `ga-0xljyj`
   and the relevant `ga-4qbgqf.2` and `ga-4qbgqf.3` criteria.
2. Verify the create gate blocks only fresh pool creates during partial demand
   reads and does not block reuse or resume paths.
3. Verify retainable and preservable behavior stays separated.
4. Check accepted sentinel/log wording.
5. Record PASS with reviewed head SHA, or CHANGES with concrete blockers.
6. Do not merge or modify code from the review bead.

### `ga-0xljyj.2`

1. Start only after `ga-0xljyj.1` records PASS.
2. Prepare the branch on the intended base without bundling unrelated changes.
3. Verify the branch still satisfies `ga-0xljyj` scope and the relevant
   `ga-4qbgqf.2`/`ga-4qbgqf.3` contracts.
4. Run `make test-fast-parallel` and `go vet ./...`.
5. Open or update the GitHub PR and record PR URL, head SHA, base, and gate
   result.
6. Route merge/MPR only if the release gate passes.

### `ga-0xljyj.3`

1. Start only after `ga-0xljyj.2` records a merged PR or equivalent landed
   commit on main.
2. Observe at least one supervisor/reconciler cycle after the landed commit is
   active, or run a controlled post-merge repro if no live partial demand read
   is present.
3. Confirm no new pool sessions are created for a template while its demand
   read is partial beyond confirmed-alive or valid in-flight capacity.
4. Capture evidence: supervisor stderr/log line, targeted command output, or
   controlled repro result.
5. Record time window, commit SHA, and residual risk.
6. If over-spawn still occurs, file a P0 `needs-architecture` bead linked to
   `ga-01yukx` and notify mayor.

## Handoff notes

- Tracker import was a no-op: no `tracker-to-beads` command or sibling tracker
  skill is installed in this worktree.
- No GitHub PR existed for `builder/ga-0xljyj-scale-check-partial-gate` at PM
  intake.
- Do not bundle Fix D or Fix E follow-ups with this release path.
- The live worktree already had unrelated local PM artifacts before this plan
  was created; this plan intentionally touches only the `ga-0xljyj` release
  path.
