# SetMetadata Clean Release Retarget

Root bead: `ga-jqvmde`
Source bead: `ga-3yad9d.1`
Reviewer handoff: `ga-2xauer`
Existing separate cleanup release: `ga-oo656x` / PR #3496
Owner: `gascity/pm`
Created: 2026-06-14

## Goal

Recover the failed SetMetadata deploy gate by splitting the release unit back
to one theme: the non-ephemeral `SetMetadata` event-recording integration
test. The agent-home worktree cleanup commits remain a separate release path
under `ga-oo656x`; they must not ride along with the SetMetadata deploy.

## Context

`ga-jqvmde` failed deploy because `origin/builder/ga-3yad9d-1` was stacked on
the agent-home worktree cleanup branch. Current branch ancestry shows:

- `0e35e5435` - SetMetadata integration test for `ga-3yad9d.1`
- `575a2f304` - ProbeDefaultBranch follow-up for Case B cleanup
- `756ef8ee3` - closed-bead agent-home worktree cleanup

The cleanup commits are already represented by `origin/builder/ga-b1huld` and
merge-request bead `ga-oo656x`. PM is not creating duplicate cleanup work.

Tracker import was a no-op for this session because no `tracker-to-beads`
skill or command is present in the worktree.

## Work Packages

1. `ga-jqvmde.1` - Builder: clean SetMetadata release branch
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: rebuild or retarget the release branch from current
     `origin/main`; preserve only the `ga-3yad9d.1` SetMetadata test diff in
     `internal/beads/native_dolt_store_integration_test.go`; exclude
     agent-home worktree cleanup files unless those changes are already on
     `origin/main`; record `git log origin/main..HEAD --oneline`, branch name,
     head SHA, focused integration test result or exact skip condition, and
     whether PR #3497 was retargeted or replaced.

2. `ga-jqvmde.2` - Validator: clean branch scope and acceptance verification
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Depends on: `ga-jqvmde.1`
   - Acceptance: verify the branch/SHA from `ga-jqvmde.1`; confirm
     `TestNativeDoltStoreRegularUpdateEventRecording` exists and meets the
     original `ga-3yad9d.1` criteria; confirm diff and ancestry exclude the
     cleanup release unless already merged to `origin/main`; record exact
     focused commands and PASS/SKIP/FAIL; route back to PM or builder if scope
     leakage remains.

3. `ga-jqvmde.3` - Deployer: deploy gate on verified clean branch
   - Route: `gascity/deployer`
   - Label: `needs-deploy`
   - Depends on: `ga-jqvmde.1`, `ga-jqvmde.2`
   - Acceptance: run the standard deploy gate only after validation passes;
     record build, smoke, vet, and focused SetMetadata test evidence; confirm
     `git log origin/main..HEAD --oneline` is SetMetadata-only; open or update
     a PR whose title and description match the SetMetadata test scope; route a
     merge request to mayor/mpr on pass; record exact failure evidence and
     route back to PM on fail.

## Dependency Graph

- `ga-jqvmde.2` depends on `ga-jqvmde.1`.
- `ga-jqvmde.3` depends on `ga-jqvmde.1`.
- `ga-jqvmde.3` depends on `ga-jqvmde.2`.

## Risks

- PR #3497 may need retargeting or replacement if GitHub still points at the
  stacked branch.
- The cleanup release may merge before this deploy retry. That is acceptable
  only if the SetMetadata branch is rebased on the new `origin/main` and its
  release diff remains SetMetadata-only.
- The live `gc` binary is still warning about local pack overrides and earlier
  event-id default failures. Downstream agents should record mail or sling
  transport failures as bead notes if event recording fails again.

## Out Of Scope

- Changes to the agent-home worktree cleanup release. That work remains under
  `ga-oo656x`.
- New architecture decisions or release process changes.
- Any PM-authored implementation or test code.
