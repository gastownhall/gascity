# Plan: main-ci-watcher release path recovery

Owner: `gascity/pm`
Created: 2026-06-19
Source beads: `ga-j6kdoo`, `ga-jqs2rp`, `ga-zzogfj`

## Goal

Recover the release for the reviewed main-ci-watcher false-positive fix without
recycling a deploy task that cannot open a PR.

The reviewed implementation is commit
`c20a109143cac522c7c57caa150b52bf3934bb1e` in
`/home/jaword/projects/gc-management`. Reviewer bead `ga-jqs2rp` passed the
change: 15/15 main-ci-watcher tests passed, with reviewed scope limited to:

- `packs/main-ci-watcher/scripts/main-ci-watcher.py`
- `packs/main-ci-watcher/tests/test-main-ci-watcher.sh`
- `packs/maintainer-pr-review/scripts/repo-policy.py`

The deploy gate on `ga-j6kdoo` failed because this is not currently a
deployable release unit: the commit exists only on local `gc-management` main,
the `gc-management` repo has no configured remote/upstream, the gascity
deployer worktree does not contain the commit, and the repo status includes
untracked `.dolt-backup/` plus the release-gate artifact. Gate artifact:
`/home/jaword/projects/gc-management/release-gates/ga-j6kdoo-main-ci-watcher-gate.md`.

## Work Packages

| Bead | Title | Route | Gate |
| --- | --- | --- | --- |
| `ga-j6kdoo.1` | Decide release path for gc-management pack changes without a configured remote | `gascity/architect` | `needs-architecture` |
| `ga-j6kdoo.2` | Prepare a deployable release unit for the reviewed main-ci-watcher fix | `gascity/builder` | `ready-to-build` |
| `ga-j6kdoo.3` | Gate and publish the deployable main-ci-watcher release unit | `gascity/deployer` | `needs-deploy` |

## Dependency Graph

```text
ga-j6kdoo.1 (architecture release-path decision)
  -> ga-j6kdoo.2 (prepare deployable release unit)
    -> ga-j6kdoo.3 (gate and publish)
```

## Acceptance Summary

`ga-j6kdoo.1` is complete when architecture records the accepted release path
for `gc-management` pack changes: local-only deployment, configured remote/PR
path, mirroring into another repository, or another explicit operator-approved
path. It must also state whether the standard deploy gate and PR requirement
apply to this repo class, and what evidence replaces them if they do not.

`ga-j6kdoo.2` is complete when builder prepares the reviewed fix as a clean
release unit using the architecture-approved target. The handoff must record
base commit, branch or artifact name, head commit, target remote if any, exact
diff scope, and test evidence for `packs/main-ci-watcher/tests/test-main-ci-watcher.sh`.
The release unit must not include `.dolt-backup/` or unrelated gate artifacts
unless architecture explicitly allows them.

`ga-j6kdoo.3` is complete when deployer gates the builder-prepared release
unit according to the accepted release path. If the path is PR-based, deployer
opens the PR and routes merge-request to mayor/mpr without merging. If the
path is local-only or otherwise non-PR, deployer records the approved
publication evidence and notifies mayor. On failure, deployer records exact
criteria and artifact path and routes back to PM.

## Handoff Notes

Do not retry `ga-j6kdoo` directly. It has already failed because the candidate
is not PR-ready. The next deploy attempt must use `ga-j6kdoo.2`'s prepared
release unit.

Builder should not change the reviewed behavior unless architecture or PM
opens a new requirement. The goal is packaging and release-path correction,
not another implementation pass.

Architecture should decide the release contract rather than asking deployer to
guess how to publish a repo with no remote/upstream. If the answer requires
operator credentials or remote configuration, that requirement should be
explicit in the architecture handoff.

## Risks

The main risk is accidentally treating a local `main` commit as a normal
PR-ready release branch. That already failed and should not be repeated. The
second risk is losing the reviewed fix while trying to move it between repos
or release paths; the builder bead pins the reviewed commit and exact file
scope. The third risk is silently publishing local-only pack changes without
operator-approved evidence; the architecture bead exists to make that policy
explicit before any retry.
