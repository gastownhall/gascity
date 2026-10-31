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

Authenticate Claude Code using its login flow, then run:

```bash
gc init --experience beginner --default-provider claude ~/bright-lights
cd ~/bright-lights
```

Init checks your provider, registers the city, and starts the orchestrator.
The beginner choice uses the default Gas City pack. It starts model sessions
on demand, uses Sonnet with medium effort for Claude, and bounds concurrent
sessions. The orchestrator and token-free maintenance can run while idle;
model work begins when you submit a task or explicitly request a session.

If preflight reports a missing dependency or login, fix the named problem and
run `gc start` from the city. Keep the existing city and its data.

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
6. Close the source task and safely retire the integrated worktree.

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
SHA. Inspect the result yourself:

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

## Continue with the same city

Keep `bright-lights` and its practice repository. Continue with
[Cities and Rigs](/tutorials/01-cities-and-rigs) to learn what you just used,
then work through tutorials 2–7 for agents, sessions, communication, formulas,
beads and orders. You can add your own repositories, parallel workers and
optional GitHub publication as you learn; there is no new starter pack to
install or migrate away from.
