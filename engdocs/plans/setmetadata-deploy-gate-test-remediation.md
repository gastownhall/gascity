# SetMetadata Deploy Gate Test Remediation

Root deploy bead: `ga-jqvmde.3`
Parent release split: `ga-jqvmde`
Owner: `gascity/pm`
Created: 2026-06-14

## Goal

Unblock the clean SetMetadata deploy retry after the standard deploy gate
failed in `make test`, while preserving the already-verified SetMetadata-only
release scope.

## Context

The clean SetMetadata release candidate is
`origin/builder/ga-jqvmde-1-setmetadata-clean` at
`6931b56ca292addefce83e99478c6cbe50a87e39`, based on `origin/main` at
`1760ea27b74c8ecb392d966c670841ef5c2d77bb`.

Validator bead `ga-jqvmde.2` passed and confirmed:

- Diff is limited to `internal/beads/native_dolt_store_integration_test.go`.
- The focused SetMetadata integration test passes.
- Agent-home worktree cleanup commits are not present on the clean branch.

Deployer then failed `ga-jqvmde.3` on deploy criterion 3 because the standard
`make test` gate exited 2. Passing evidence already recorded by deployer:

- `go vet ./...` passed.
- Build and smoke checks passed.
- Focused integration test passed:
  `go test -tags=integration ./internal/beads -run '^TestNativeDoltStoreRegularUpdateEventRecording$' -count=1 -v`.

Failing standard test names recorded by deployer:

- `TestCmdCityStatusJSONConfigErrorIsStructured`
- `TestCmdMailReplyExecProviderNotifyWithoutCityWarnsAndSendsReply`
- `TestDoPrimeStrictNoCity`
- `TestDoStartRequiresInitializedCity`
- `TestEventsJSONFlagIsSilentNoOp`
- `TestResolveEventsPath_NoSourceReturnsError`
- `TestRigAnywhere_ResolveContext/failure_nothing_matches`
- `TestRunDashboardServeAllowsNoCityWithAPIOverride`
- `TestRunDashboardServeAllowsNoCityWithSupervisor`

Deployer artifact: `release-gates/ga-jqvmde-3-setmetadata-gate.md` on local
branch `deploy/ga-jqvmde-3-gate` at `1d1e4bc61`. Observable test log:
`/tmp/gascity-test.jsonl.ZgkWG7`.

Tracker import was a no-op for this PM pass because no `tracker-to-beads`
skill or command is present in the worktree or rig path.

## Work Packages

1. Builder: standard gate test remediation
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Parent: `ga-jqvmde`
   - Source deploy bead: `ga-jqvmde.3`
   - Acceptance: reproduce the listed `make test` failures from the deployer
     handoff; determine whether they fail on current `origin/main`, the clean
     SetMetadata candidate, or both; apply the minimal fix needed to make the
     standard deploy test gate pass without broadening the SetMetadata release
     scope; record branch name, base SHA, head SHA, exact failing/passing
     commands, and any files changed; if the failure requires a release-policy
     or architecture decision rather than a contained fix, stop and route to
     PM or architect with evidence.

2. Validator: standard gate verification
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Parent: `ga-jqvmde`
   - Source deploy bead: `ga-jqvmde.3`
   - Depends on: builder remediation
   - Acceptance: verify the builder branch or corrected release candidate;
     rerun `make test` or the documented standard test command and confirm the
     previously failing tests pass; rerun `go vet ./...`; rerun the focused
     SetMetadata integration test; confirm the final deploy candidate remains
     SetMetadata-only except for any separately approved gate-remediation unit;
     record exact commands, PASS/SKIP/FAIL, branch, base SHA, and head SHA.

3. Deployer: existing deploy retry
   - Route: `gascity/deployer`
   - Label: `needs-deploy`
   - Existing bead: `ga-jqvmde.3`
   - Depends on: builder remediation and validator verification.
   - Acceptance: keep the current `ga-jqvmde.3` deploy acceptance, but retry
     only after the standard test remediation and validation pass. On pass,
     open or update the SetMetadata-only PR and route merge request to
     mayor/mpr. On fail, record exact evidence and route back to PM.

## Dependency Graph

- Validator remediation depends on builder remediation.
- Existing deploy bead `ga-jqvmde.3` depends on builder remediation.
- Existing deploy bead `ga-jqvmde.3` depends on validator remediation.

The remediation beads are children of closed split bead `ga-jqvmde`, not
children of `ga-jqvmde.3`, so the deploy bead can depend on them without
creating a parent-child dependency cycle.

## Risks

- The failed tests may reflect a current `origin/main` baseline problem rather
  than the clean SetMetadata branch. Builder must record that distinction so
  release ownership stays clear.
- A fix for the standard gate may be a separate release unit. Validator must
  explicitly state whether the SetMetadata PR remains single-theme or whether
  PM needs another split before deploy.
- Previous `gc sling` and `gc mail send` attempts hit `HY000 Field id has no
  default value` while recording wisp events. Durable routing must be recorded
  on beads even if wakeup transport fails again.

## Out Of Scope

- Agent-home worktree cleanup behavior, tracked separately under `ga-oo656x`
  and PR #3496.
- PM-authored implementation or test changes.
- Retrying deploy before the standard gate failure is resolved and validated.
