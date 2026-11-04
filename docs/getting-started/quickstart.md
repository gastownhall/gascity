---
title: Quickstart
description: Complete one local change with implementation, independent review, tests, and integration.
---

Start with [Installation](/getting-started/installation). You need `gc`, Git,
Python 3, the documented runtime dependencies, and one authenticated coding
provider. This walkthrough uses Claude Code. You do not need GitHub, a remote
repository, or the `gh` CLI.

<Tip>
Oh My Zsh can alias `gc` to `git commit`. If `gc version` opens Git, use
`command gc` and remove that alias after Oh My Zsh loads. See
[Troubleshooting](/getting-started/troubleshooting#oh-my-zsh-git-plugin-hides-gc).
</Tip>

## 1. Choose the beginner experience

Authenticate Claude Code using its login flow and complete the one-time
[Dolt author setup](/getting-started/installation#configure-dolts-author-identity),
then run:

```bash
gc init --experience beginner --default-provider claude ~/bright-lights
cd ~/bright-lights
```

Init checks your provider, registers the city, and starts the orchestrator.
The beginner choice uses the default Gas City pack. It starts model sessions
on demand and preserves your normal provider and capacity choices.
The orchestrator and token-free maintenance can run while idle;
model work begins when you submit a task or explicitly request a session.

If preflight reports a missing dependency or login, apply its named remedy.
If the city directory was not created, repeat the init command above. If it
already exists, run `gc start` from that city, preserving its configuration
and data.

## 2. Submit your first local task

```bash
gc gc tutorial start
```

This creates a small practice repository at `projects/practice`, registers it
as a rig, and records one task: fix a greeting function with two failing tests.
It records registration infrastructure before pinning the task input. It
refuses an unrelated existing practice directory and preserves retries.

The default pack runs the complete task:

1. Pin the input commit and create an owned, isolated worktree.
2. Implement and commit the fix.
3. Have a separate worker review the actual diff.
4. Apply at most two repair rounds if review or verification fails.
5. Test the approved commit, then integrate that exact commit locally.
6. Safely retire the integrated worktree, then let native finalization close
   the source task after all required stages pass.

No push or pull request is part of this workflow. A failed check leaves useful
evidence and recoverable work instead of calling the task complete.

## 3. Watch and verify the result

```bash
gc gc tutorial status
gc status
```

The tutorial prints the source task ID. While it is open, implementation,
review or verification is still pending. On success, tutorial status verifies
that the recorded integrated commit belongs to the local branch and prints its
SHA. It also checks that the workflow passed and its owned worktree was
cleaned up. Inspect the result yourself:

```bash
cd projects/practice
python3 -m unittest discover -s tests -v
git log --oneline -3
```

Both greeting tests should pass. The source task closes after local integration;
a closed workflow step alone is not a delivered change.

## 4. Pause and resume

From the city directory:

```bash
gc stop
gc start
```

Work and commit evidence persist across sessions. Stop when you want the city
paused; start it to resume. A failed stage remains visible for diagnosis. Do not
delete a worktree or close source work to clear an error. Use the source task
ID printed by the tutorial with `gc bd show <task-id>` to inspect it.

## Recover a stopped lesson

Inspect the lesson from the city directory:

```bash
gc gc tutorial status --lesson greeting
gc status
```

An environment blocker keeps the existing work waiting. Repair the reported
prerequisite and resume a paused city with `gc start`.

When review or verification exhausts its two repair rounds, the workflow stops
and the source task stays open. Correct the reported problem, then request a
new bounded attempt:

```bash
gc gc tutorial retry --lesson greeting
```

Use `api` or `client` for those lessons. Retry keeps the source task and recorded
input commit, and retains the previous unfinished worktree. Repeating it while
work is active observes that attempt. If preparation stopped before selecting
an input, retry pins the current clean default-branch commit.

A dirty or advanced checkout, a changed verification contract, or an already
integrated change requires attention to the existing work before replacement.
Keep that work and inspect the source ID shown by status:

```bash
gc --rig practice bd show <source-id> --json
```

## Use your own repository

You can take the delivery path directly to an existing project; the practice
repository is optional. From the city directory, register a clean checkout:

```bash
gc rig add ~/code/my-project --name project --prefix proj
```

Gas City records the repository's default branch. Check out that branch before
submitting work. Review the registration changes with Git and commit the
infrastructure files you want tracked; task setup refuses a dirty checkout.
If the project already has a beads database, use the adoption preview below
before registering it.

Create a task with specific acceptance criteria, then submit its printed ID:

```bash
gc --rig project bd create "Fix the reported bug" --description "Describe the failing behavior and required result"
gc sling <task-id> --var verify_command="your project's verification command"
```

Choose a real verification command, such as `npm test` or the project's Bazel
target. The imported pack supplies the delivery route and workflow; no pack
editing or coordinator conversation is required. Missing verification input
is an error rather than permission to skip tests.

The workflow fetches the configured default branch from `origin` when present.
It includes commits already integrated locally, even if they are not pushed.
Diverged branches or a failed fetch stop setup visibly; reconcile them explicitly
and retain the task. A repository without `origin` uses its local default branch.
Review, verification, integration and cleanup follow the same rules as the
practice task. Publication remains a separate choice.

## Adopt an existing beads queue

Keep a recoverable backup of your existing beads database before adoption.
A JSONL export preserves issue records, dependencies and comments, but it is
not a full database backup. Use the installed Beads `backup` commands when
you need Dolt branches, commit history and working-set state as well.

From your Gas City directory, preview the existing project's registration:

```bash
gc rig add ~/code/my-project --name project --adopt --dry-run
```

The preview checks the path, existing queue prefix and proposed city topology.
It reports the proposed rig, prefix and default branch without changing your
city files, repository, queue, hooks or running sessions. Pack resolution may
populate a derived download cache. A prefix collision is an error; keep the
existing issue IDs and resolve the conflicting registration before proceeding.

Gas City keeps the existing queue's prefix, including when Beads stores it in
its database. You do not need to add the prefix to a YAML file. If the queue's
database is stopped, run `bd ping` in that repository before previewing;
the preview itself does not start it.

If Gas City refuses an unproven native binding, keep the original `.beads`
directory and database. Follow [Unproven Native Beads Binding](/getting-started/troubleshooting#unproven-native-beads-binding)
to inspect and recover the existing binding before retrying adoption. A proposed
city endpoint does not prove ownership of the old database.

Read the plan, then apply the same command without `--dry-run`:

```bash
gc rig add ~/code/my-project --name project --adopt
gc doctor --json
```

Inspect existing work with `gc --rig project bd list --all`. Its issue IDs,
dependencies, comments and statuses should remain intact. Use the same source
task submission and verification path described above. Adoption does not mean
old work is complete or permission to start every queued task.

## Continue with the same city

Keep `bright-lights` and its practice repository. Continue with
[Cities and Rigs](/tutorials/01-cities-and-rigs) to learn what you just used,
then work through tutorials 2–7 for agents, sessions, communication, formulas,
beads and orders. You can add your own repositories, parallel workers and
optional GitHub publication as you learn; there is no new starter pack to
install or migrate away from.
