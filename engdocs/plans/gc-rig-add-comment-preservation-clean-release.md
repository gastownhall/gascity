# gc rig add comment preservation clean release

## Goal

Ship the `gc rig add` `city.toml` comment-preservation fix from `ga-5ewyrj`
without bundling the unrelated themes currently present in PR #3402.

## Context

The original deploy bead `ga-un5mg3` failed the release gate because its
candidate branch, `builder/ga-frmdxd.3`, contains multiple independent release
themes:

- `gc rig add` comment preservation
- emergency relay changes
- supervisor HTTP doctor/API schema changes
- hook and city-discovery fixes
- generated dashboard type changes
- prior release-gate artifacts

The reviewed commit `eccb57af6` covers only the intended rig-add fix across:

- `cmd/gc/cmd_rig.go`
- `cmd/gc/cmd_rig_test.go`
- `internal/config/site_binding.go`

A clean cherry-pick onto current `origin/main` may produce a new commit hash.
That is acceptable only if the resulting PR keeps the same three-file scope and
records the final clean-branch commit hash for validator and deployer.

PR #3402 remains under MPR hold `ga-ohfa8l` and is not the release vehicle for
this single-bead deploy.

## Work Packages

1. `ga-un5mg3.1` - Create clean single-feature PR for gc rig add comment
   preservation.
   - Route: `gascity/builder`
   - Label: `ready-to-build`
   - Acceptance: clean branch from current `origin/main`, PR URL/branch/commit
     recorded, diff limited to the three rig-add files unless PM is re-engaged,
     regression test present and relevant Go tests passing, PR #3402 not reused.

2. `ga-un5mg3.2` - Validate clean gc rig add release scope and regression
   evidence.
   - Route: `gascity/validator`
   - Label: `needs-tests`
   - Depends on: `ga-un5mg3.1`
   - Acceptance: confirms unrelated PR #3402 themes are absent, records checked
     PR URL/branch/commit, verifies regression evidence, records test results,
     returns scope violations to PM and builder instead of deploy.

3. `ga-un5mg3.3` - Run deploy gate on clean gc rig add comment-preservation PR.
   - Route: `gascity/deployer`
   - Label: `needs-deploy`
   - Depends on: `ga-un5mg3.2`
   - Acceptance: deploy gate runs only on the clean PR, not PR #3402; build,
     smoke, and release-scope evidence are recorded; on pass, merge-request is
     routed to mayor/mpr for the clean PR only; report notes that `ga-ohfa8l`
     remains separate.

## Dependency Graph

`ga-un5mg3.1` -> `ga-un5mg3.2` -> `ga-un5mg3.3`

## Non-Goals

- Do not clear the MPR hold for PR #3402 as part of this release split.
- Do not convert PR #3402 into the release vehicle for `ga-un5mg3`.
- Do not make architecture decisions about the unrelated bundled themes.
