# Reset-Main Orphan Guard Handoff Plan

Root bead: `ga-h41dup`
Source: investigator root-cause report, prioritized by mayor mail `gm-wisp-smo71`
Priority: P1

## Goal

Move the completed root-cause investigation for `--reset-main` branch
orphaning into an implementation path that first settles cross-rig guardrails,
then lands the surgical fix, then verifies the regression coverage.

## Why This Matters

`freshen_to_main()` in the shared `gc-management` pack worktree setup can hard
reset a worktree whose current branch has commits not yet on the selected base.
That can orphan reviewed commits and has already produced repeated city-wide
waste around review, deploy, and stale worktree recovery.

## Work Packages

| Bead | Route | Label | Acceptance Summary |
| --- | --- | --- | --- |
| `ga-h41dup.1` | `gascity/architect` | `needs-architecture` | Confirm the implementation boundary for `packs/gastown/scripts/worktree-setup.sh`, define guardrails for ahead-of-base handling and `.worktree-stale`, and settle the HQ/shared-clone isolation hazard before build starts. |
| `ga-h41dup.2` | `gascity/builder` | `ready-to-build` | Implement the architecture-approved `freshen_to_main()` guard so ahead commits are preserved, `.worktree-stale` records the skip reason, normal at-or-behind-main reset behavior still works, and focused tests cover the falsifiable regression. |
| `ga-h41dup.3` | `gascity/validator` | `needs-tests` | Independently verify the regression test, guarded behavior, unchanged safe reset path, quality gates, and any remaining live-risk follow-up for `review/ga-ieizh2` or `ga-p0u752`. |

## Dependency Graph

```text
ga-h41dup.1 (architecture guardrails)
  -> ga-h41dup.2 (builder implementation)
  -> ga-h41dup.3 (independent validation)
```

## Guardrails

- Do not start the builder bead before architecture has resolved the
  HQ/shared-clone isolation concern called out by mayor.
- Keep the root fix focused on `freshen_to_main()` preserving ahead commits.
- Treat branch-tip preflight defense-in-depth as a separate follow-up unless
  architecture explicitly scopes it into the root fix.
- Do not add role-specific behavior to SDK code.
- Preserve existing at-or-behind-main reset behavior for intended
  `--reset-main` template worktrees.

## Out Of Scope

- New orchestration policy or role decision logic.
- Broad deploy gate preflight changes beyond the root fix.
- Cleanup of unrelated stale worktrees or unrelated orphaned commits.
