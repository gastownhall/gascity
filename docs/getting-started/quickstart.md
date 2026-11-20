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

## Create a city without the guided experience

The guided practice task is optional. To create a city and send your own work
through a formula directly, you need `gc` (`brew install gascity` brings `bd`,
`dolt`, `tmux`, `jq`, and `flock` with it) and a coding agent CLI that is
already logged in. The examples use Claude Code and a separate `~/my-city`.

### 1. Give Dolt your identity

Gas City stores work as beads in Dolt, and Dolt needs an author. `gc init`
stops with "startup is blocked by Dolt author identity" without one.

Git needs an identity first: the commands below copy it, and step 3 commits
with it. If `git config --global user.name` or `git config --global user.email`
prints nothing, set it with `git config --global user.name "Your Name"` or
`git config --global user.email "you@example.com"`, using your own name and
email.

Then copy your git identity into Dolt wherever Dolt's name or email is missing
or empty:

```bash
[ -n "$(dolt config --global --get user.name)" ]  || dolt config --global --add user.name  "$(git config --global user.name)"
[ -n "$(dolt config --global --get user.email)" ] || dolt config --global --add user.email "$(git config --global user.email)"
```

### 2. Create a City

```bash
gc init ~/my-city --template gascity --default-provider claude
```

`gc init` bootstraps the city directory, registers it with the supervisor, and
starts the orchestrator in the background. The defaults are the recommended
setup; use `--default-provider codex` (or `gemini`) for a different agent CLI.
The dashboard is at `http://127.0.0.1:8372/`.

A new city takes a few minutes to finish setting up. Until it does, these
signals are normal and need no action:

- `gc init`'s output can end at `Adopting sessions...`.
- `gc status` can list agents as `unknown (partial status)` and print
  `bead cache unavailable` or `session snapshot timed out`.
- `gc doctor` can report the store as not running.
- A dashboard run page can say "run projection is warming"; refresh it after a
  minute.

### 3. Add a Rig

```bash
mkdir -p ~/hello-world && cd ~/hello-world
git init && git commit --allow-empty -m "initial commit"
cd ~/my-city
gc rig add ~/hello-world
```

A rig is a project directory registered with the city. It gets its own beads
database, hook installation, and routing context.

### 4. Send Work Through a Formula

```bash
gc sling hello-world/claude "Create hello.sh that prints hello world and commit it" --on mol-do-work
```

The target is the rig, then the provider: if you chose `codex` or `gemini` in
step 2, sling to `hello-world/codex` or `hello-world/gemini`.

`gc sling` creates a bead for the task and runs the `mol-do-work` formula on
it: one agent reads the task, does it, and closes it. It prints a `Dashboard:`
link to the run:

```text
Dashboard: http://127.0.0.1:8372/city/my-city/runs/gcg-1025
```

That line is the result of getting Gas City running. An agent doing this setup
for you should end its final message with it exactly as printed. `gc sling`
leaves the line out when it cannot reach the dashboard, but the run still
started: its `Attached workflow <ID>` line names the run, and the link is
`http://127.0.0.1:8372/city/my-city/runs/<ID>`.

### 5. Watch It Finish

Open the link, or pass the run ID (the last part of the link) to `gc bd show`.
Its `status:` line reads `closed` when the run is done, usually within a minute
or two, and `hello.sh` is committed in `~/hello-world`:

```bash
gc bd show gcg-1025
```

This is the smallest formula: one bead, one agent. Gas City's real power is
orchestration -- a **formula** (the method for getting a job done) that the
**orchestrator** runs as a graph: decomposing the work into beads, fanning the
ready ones out to many agents at once, gating each step on its dependencies,
retrying failures, and driving the whole thing to completion outside your
session. See [Formulas](/tutorials/05-formulas) and
[Orders](/tutorials/07-orders), which trigger formulas on a schedule or event.
For a fuller walkthrough of cities and rigs, continue to
[Tutorial 01](/tutorials/01-cities-and-rigs).

## Continue with the same city

Keep `bright-lights` and its practice repository. Continue with
[Cities and Rigs](/tutorials/01-cities-and-rigs) to learn what you just used,
then work through tutorials 2–7 for agents, sessions, communication, formulas,
beads and orders. You can add your own repositories, parallel workers and
optional GitHub publication as you learn; there is no new starter pack to
install or migrate away from.
