# Hook Claim Clean Release Split

Root bead: `ga-sxe4sc`
Source implementation bead: `ga-an3le0`
Reviewed commit: `3ec937bd30c325e8f827c47d92babd50e97e9a45`
Failed branch: `rebase/extmsg-subscribe`
Conflicting release theme: extmsg connected-client work from PR #3657
Owner: `gascity/pm`
Created: 2026-06-30

## Goal

Unblock deployment of the reviewed hook-claim pool graph.v2 root nudge fix
without shipping the unrelated extmsg connected-client SSE/API/docs/dashboard
work that is already represented by PR #3657.

The deploy gate failed `ga-sxe4sc` because the candidate branch bundled more
than one release theme. PM is splitting the release packaging: first isolate the
reviewed hook-claim fix on a clean branch, then run the standard deploy gate on
that clean release unit.

## Context

`ga-an3le0` reviewed and passed the hook-claim fix at
`3ec937bd30c325e8f827c47d92babd50e97e9a45`. The deployer rejected
`ga-sxe4sc` because `rebase/extmsg-subscribe` also contains independent extmsg
connected-client work from `deploy/ga-j9xfm0-extmsg-subscribe-clean` / PR
#3657.

Tracker import was a no-op for this session because no `tracker-to-beads` skill
or command is present in the worktree.

## Work Packages

| Bead | Route | Label | Purpose |
| --- | --- | --- | --- |
| `ga-sxe4sc.1` | `gascity/builder` | `ready-to-build` | Create a clean single-theme release branch for the reviewed hook-claim fix. |
| `ga-sxe4sc.2` | `gascity/deployer` | `needs-deploy` | Run the deploy gate on the clean branch and open the PR on pass. |

## Acceptance

`ga-sxe4sc.1` is complete when a branch based on current `origin/main` contains
only the hook-claim continuation nudge fix represented by reviewed commit
`3ec937bd30c325e8f827c47d92babd50e97e9a45`. Its bead notes must record branch
name, head SHA, `git log origin/main..HEAD --oneline`, changed files, and
focused hook-claim/nudge test results or exact failure context. The diff must
exclude extmsg connected-client SSE/API/docs/dashboard changes and unrelated PR
#3657 content.

`ga-sxe4sc.2` is complete when the deployer runs the standard deploy gate on
the clean branch from `ga-sxe4sc.1`, confirms the release diff is hook-claim
only, records build/smoke/test/vet evidence, opens or updates a PR on pass,
routes a merge request to mayor/mpr, and reports the PR URL plus gate result to
mayor. The deployer must not merge the branch to main directly.

## Dependency Graph

`ga-sxe4sc.2` depends on `ga-sxe4sc.1`.

The deployer bead must remain blocked until the builder bead records the clean
branch name and head SHA.

## Risks

The hook-claim commit may require mechanical conflict resolution when replayed
onto current `origin/main`. If that resolution changes product behavior beyond
the reviewed fix, builder should route the bead back for review instead of
treating it as release packaging.

PR #3657 may merge before the clean branch is created. That is acceptable only
if the clean branch is rebased on the resulting `origin/main` and its remaining
diff is still hook-claim only.

## Out Of Scope

Extmsg connected-client SSE/API/docs/dashboard deployment remains represented
by PR #3657 and must not be bundled into this release unit.

PM is not authoring implementation code, tests, or architecture decisions for
this split.
