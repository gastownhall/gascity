---
title: Configure Managed Worktrees
description: Choose workspace inputs and reuse policies for pack workflows.
---

Packs decide where work runs and how its checkout is prepared. Managed
worktrees give each work item its own branch and checkout, with checks that
the workspace belongs to that item and contains its required starting commit.

This guide covers workspace choices, the input to supply, and the behavior
you can expect from `gc worktree`. See [Understanding Packs](/guides/understanding-packs)
for importing and customizing a pack, and the
[worktree CLI reference](/reference/cli#gc-worktree) for the complete flag list.

## Choose A Workspace Approach

| Approach | Where you configure it | Behavior |
|---|---|---|
| Shared checkout | The worker's `work_dir` | Work runs in the selected directory. Tasks share its files and branch. |
| Git worktree | A pack's preparation script or formula step runs `git worktree add` | Tasks have separate checkouts. The pack implements input selection, ownership and reuse checks. |
| Managed worktree | A pack's preparation script or formula step calls `gc worktree ensure` and `gc worktree verify` | Each workspace has an attached branch and recorded ownership. Reuse checks its identity and required input commit. |

Setting `work_dir` chooses a working directory; it does not create a worktree
or enable the managed-worktree checks. Check the preparation step in the pack
version you use to see which approach it implements.

## Choose The Input Commit

Supply `--base` with the commit the task needs to start from. It must be
available in the local repository. For independent work, this might be a
selected commit from the project's default branch. For dependent work, it
should be the result of integrating the accepted prerequisite outputs.

For a fixed input with later cleanup after merge, supply the merge branch ref
as `--base` and the selected input SHA as `--base-sha`. At creation, the ref
must resolve to that SHA; the commit is recorded for later reuse checks.
Keep the original ref and SHA when verifying or reusing the workspace. The
worktree commands do not fetch or integrate prerequisite outputs for you.

## Prepare A Managed Workspace

Call `gc worktree ensure` from the pack's workspace preparation step. Give each
work item a distinct path and branch, and keep its ownership values consistent
when verifying or reusing it.

For example, replace these paths, input SHA and work identity with the values
selected by your pack:

```bash
gc worktree ensure \
  --repo /path/to/project --root /path/to/workspaces \
  --path /path/to/workspaces/task-c --branch work/task-c \
  --base refs/heads/integration --base-sha REPLACE_WITH_INPUT_COMMIT_SHA \
  --bead task-c --store-ref city:my-city \
  --creator do-work --owner workspace-provisioner \
  --generation attempt-1 --json
```

The path must be a direct child of `--root`. The bead and store identify the
task; creator and owner identify its provisioner. Generation must match when
the workspace is reused. Each creation also gets an attempt ID, which binds
cleanup to that specific workspace.

The command returns a JSON report with the workspace and its recorded
provenance. The preparation step should retain that report and give the worker
the resulting workspace path only after the command succeeds.

| Option | Use it to |
|---|---|
| `--base` | Select the required starting commit or a locally resolvable ref. |
| `--base-sha` | Require the expected input SHA at creation and check it on reuse. |
| `--dry-run` | Plan creation or reuse without changing the repository or filesystem. The same input checks apply. |
| `--json` | Return a structured report for the pack's next step. |

Use `gc worktree verify` with the matching path, branch, ownership and input
flags to check a workspace without changing it. See the
[configuration reference](/reference/config) for `work_dir` path handling and
[Understanding Formulas](/guides/understanding-formulas) for composing preparation
and implementation steps.

## Reuse And Recovery

A managed workspace is reusable when its identity matches and its current
commit descends from the recorded input. Additional commits remain usable.
If that input is missing from the branch's history, verification and reuse
fail, including dry-run planning. The refusal leaves existing branches and
working files intact.

If you see `HEAD ... does not contain recorded base ...`:

1. Inspect the branch and any uncommitted work.
2. Create a distinct workspace and branch from the required input, or review
   and integrate that input into the existing branch.
3. Verify the selected workspace with its matching identity and input before
   continuing.

Changing the workflow's input requires an explicit workspace decision. The
commands do not automatically reset a branch or update its recorded base.

## Guarantees For Dependent Work

Managed-worktree verification checks the repository, path, attached branch,
recorded ownership and input ancestry. It proves that the required commit is
in the checkout's history.

The pack still controls two other parts of delivery:

- **Input selection:** wait for the required work, select accepted outputs,
  integrate them, and supply that exact result as the next task's input.
- **Behavior:** run project tests and any task-specific checks against the
  resulting commit, and bind review to that same commit. A later change needs
  fresh evidence; retaining the input in history does not ensure its behavior
  remains intact.

For removal, use [gc worktree cleanup](/reference/cli#gc-worktree-cleanup) with
the attempt ID returned during creation. Cleanup also checks ownership and Git
safety. It refuses dirty workspaces, commits reachable from no branch, tag or
remote-tracking ref, and HEADs not merged into `--base`.

Use the same base ref and input SHA supplied at creation. Cleanup checks the
current tip of that ref for the merge. A fixed SHA supplied as `--base` remains
the original commit, so cleanup refuses later commits even if they were merged
elsewhere. Choose the merge branch ref at creation when post-merge cleanup is
needed.
